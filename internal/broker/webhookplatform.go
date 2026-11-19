package broker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/webhook"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// newWebhookPlatform builds the webhook platform of conn (internal/chat/webhook) from its spec.webhook and the one key of its credentials
// Secret that its verify scheme reads (v1alpha1.CredentialKeys). Its requests reach it on the chat listener, at
// /chat/webhook/<namespace>/<name>, and it records each answer through telemetry.WebhookRecorder, on the broker's instruments in ctx:
// agentkourier_webhook_requests_total, agentkourier_webhook_ack_duration_seconds and
// agentkourier_webhook_last_accepted_timestamp_seconds. Any error is a wiring error, which no retry mends: no spec.webhook, or a spec or credential the platform refuses. A credential it
// refuses (empty, a control character such as a trailing newline, a token that is too short) is named by its Secret and key, never by its
// value.
func newWebhookPlatform(ctx context.Context, conn Connection, log *slog.Logger, onConnection func(connected bool)) (builtPlatform, error) {
	cfg, err := webhookConfig(conn)
	if err != nil {
		return builtPlatform{}, err
	}
	cfg.Recorder = telemetry.WebhookRecorder(ctx, cfg.Connection)
	cfg.OnConnection, cfg.Logger = onConnection, log
	p, err := webhook.New(cfg)
	if err != nil {
		return builtPlatform{}, fmt.Errorf("%w: %w", ErrWiring, err) // New names the connection
	}
	// The platform sends nothing, so it has no transport to close.
	return builtPlatform{Platform: p, closeIdle: func() {}}, nil
}

// checkWebhookConnection builds the webhook platform of conn and drops it, for Run to refuse at start, on every replica, a connection
// whose platform the leader could not build: a standby would otherwise start and say Ready, and fail only when it took over.
func checkWebhookConnection(conn Connection) error {
	cfg, err := webhookConfig(conn)
	if err != nil {
		return err
	}
	if _, err := webhook.New(cfg); err != nil {
		return fmt.Errorf("%w: %w", ErrWiring, err) // New names the connection
	}
	return nil
}

// webhookConfig maps conn's spec.webhook onto the platform's Config: the API's names onto the platform's, the scheme's key of the
// credentials onto the verify secret, with the Secret and key that an error names, and the API's channel shape (ValidWebhookChannel), so
// that the platform and the Bindings hold one shape. The Recorder, OnConnection and Logger are the caller's.
func webhookConfig(conn Connection) (webhook.Config, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	spec := conn.Webhook
	if spec == nil {
		return webhook.Config{}, fmt.Errorf("%w: ChatConnection %s is a webhook connection with no spec.webhook", ErrWiring, name)
	}
	key := v1alpha1.WebhookSecretKey
	if spec.Verify.Scheme == v1alpha1.WebhookToken {
		key = v1alpha1.WebhookTokenKey
	}
	cfg := webhook.Config{
		Connection: name,
		Identity:   spec.Identity,
		Verify: webhook.Verify{
			Scheme: webhook.Scheme(spec.Verify.Scheme), Header: spec.Verify.Header, Prefix: spec.Verify.Prefix,
			Secret: webhook.Credential{Secret: conn.Secret, Key: key, Value: conn.Credentials[key]},
		},
		Channel:      webhook.Channel{From: spec.Channel.From, Value: spec.Channel.Value},
		Actor:        webhook.Actor{From: spec.Actor.From, None: spec.Actor.None, KeepMissing: spec.Actor.WhenMissing == v1alpha1.WebhookMissingKeep},
		ValidChannel: v1alpha1.ValidWebhookChannel,
	}
	if ts := spec.Verify.Timestamp; ts != nil {
		var unit time.Duration // a unit the API does not have is no unit, which New refuses
		switch ts.Unit {
		case v1alpha1.WebhookUnitSeconds:
			unit = time.Second
		case v1alpha1.WebhookUnitMillis:
			unit = time.Millisecond
		}
		cfg.Verify.Timestamp = &webhook.Timestamp{From: ts.From, Unit: unit, Window: ts.Window.Duration}
	}
	for _, f := range spec.Fields {
		cfg.Fields = append(cfg.Fields, webhook.Field{Name: f.Name, From: f.From, Header: f.Header, ID: f.IsID()})
	}
	if th := spec.Thread; th != nil {
		cfg.Thread = &webhook.Thread{Field: th.Field, Prefix: th.Prefix}
	}
	if d := spec.Delivery; d != nil {
		cfg.DeliveryHeader = d.Header
	}
	return cfg, nil
}
