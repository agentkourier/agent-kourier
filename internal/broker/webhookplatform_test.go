package broker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/webhook"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// fakeWebhookSecret is the fake HMAC secret of the webhook connections of these tests.
const fakeWebhookSecret = "fake-webhook-secret"

// webhookSpec is a spec.webhook that the API and the platform take: GitHub's shape, with every optional part set.
func webhookSpec() *v1alpha1.WebhookSpec {
	return &v1alpha1.WebhookSpec{
		Identity: "acme-review-agent",
		Verify: v1alpha1.WebhookVerify{Scheme: v1alpha1.WebhookHMACSHA256, Header: "X-Hub-Signature-256", Prefix: "sha256=",
			Timestamp: &v1alpha1.WebhookTimestamp{From: "/ts", Unit: v1alpha1.WebhookUnitMillis, Window: metav1.Duration{Duration: time.Minute}}},
		Channel:  v1alpha1.WebhookChannel{From: []string{"/repository/full_name"}},
		Actor:    v1alpha1.WebhookActor{From: []string{"/sender/login"}, WhenMissing: v1alpha1.WebhookMissingKeep},
		Thread:   &v1alpha1.WebhookThread{Field: "number", Prefix: "pr-"},
		Fields:   []v1alpha1.WebhookField{{Name: "number", From: []string{"/number"}, Kind: v1alpha1.WebhookKindID}, {Name: "event", Header: "X-GitHub-Event"}},
		Delivery: &v1alpha1.WebhookDelivery{Header: "X-GitHub-Delivery"},
	}
}

// webhookConn is the webhook connection ns/name with spec and the secret value of its scheme's key.
func webhookConn(name string, spec *v1alpha1.WebhookSpec, value string) Connection {
	key := v1alpha1.CredentialKeys(v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWebhook, Webhook: spec})[0]
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: "ns", Name: name}, Platform: v1alpha1.PlatformWebhook, Secret: "ns/" + name + "-webhook",
		Credentials: map[string]secret.Value{key: secret.New(value)}, Webhook: spec}
}

// A webhook connection is built as the webhook platform, which takes its requests on the chat listener: it offers a handler, and a broker
// with no chat listener refuses to build it with a wiring error, since the service could not reach it.
func TestNewPlatformBuildsAWebhookConnectionOnTheChatListener(t *testing.T) {
	conn := webhookConn("github", webhookSpec(), fakeWebhookSecret)
	var none RunConfig
	if built, err := none.newPlatform(t.Context(), conn, nil, nil, nil, nil); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") || built.Platform != nil {
		t.Errorf("newPlatform(webhook) with no chat listener = %+v, %v; want a wiring error that names AGENTKOURIER_CHAT_LISTEN", built, err)
	}
	cfg := RunConfig{ChatListener: someListener(t)}
	built, err := cfg.newPlatform(t.Context(), conn, nil, nil, nil, nil)
	if err != nil || built.closeIdle == nil {
		t.Fatalf("newPlatform(webhook) = %+v, %v; want the webhook platform", built, err)
	}
	if _, ok := built.Platform.(*webhook.Platform); !ok {
		t.Fatalf("newPlatform(webhook) built %T; want *webhook.Platform", built.Platform)
	}
	if _, ok := receiverOf(built.Platform); !ok {
		t.Error("the webhook platform offers no handler for the chat listener")
	}
	built.closeIdle() // nothing to close, and it must not panic
	noSpec := conn
	noSpec.Webhook = nil
	if _, err := cfg.newPlatform(t.Context(), noSpec, nil, nil, nil, nil); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "no spec.webhook") {
		t.Errorf("newPlatform(webhook with no spec.webhook) = %v; want a wiring error that says so", err)
	}
}

// A credential the platform refuses (spec D55) is a wiring error that names its Secret and key and never holds its value: one with
// a control character, most often the trailing newline of a file read into the Secret, which is refused and not trimmed; an empty one; and a
// token shorter than the token scheme takes.
func TestAWebhookCredentialThePlatformRefusesIsAWiringErrorThatNamesTheSecretAndKey(t *testing.T) {
	token := webhookSpec()
	token.Verify = v1alpha1.WebhookVerify{Scheme: v1alpha1.WebhookToken, Header: "X-Gitlab-Token"}
	tests := []struct {
		name  string
		spec  *v1alpha1.WebhookSpec
		value string
		key   string
		want  error
	}{
		{"a trailing newline", webhookSpec(), fakeWebhookSecret + "\n", v1alpha1.WebhookSecretKey, webhook.ErrControlCharacter},
		{"a carriage return", webhookSpec(), "fake\rwebhook-secret", v1alpha1.WebhookSecretKey, webhook.ErrControlCharacter},
		{"an empty value", webhookSpec(), "", v1alpha1.WebhookSecretKey, secret.ErrEmptyValue},
		{"a short token", token, "fake-short-token", v1alpha1.WebhookTokenKey, webhook.ErrTokenTooShort},
	}
	cfg := RunConfig{ChatListener: someListener(t)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := webhookConn("hooks", tt.spec, tt.value)
			for what, err := range map[string]error{
				"newPlatform":        func() error { _, err := cfg.newPlatform(t.Context(), conn, nil, nil, nil, nil); return err }(),
				"checkOwnConnection": cfg.checkOwnConnection(conn),
			} {
				if !errors.Is(err, ErrWiring) || !errors.Is(err, tt.want) {
					t.Fatalf("%s = %v; want a wiring error wrapping %v", what, err, tt.want)
				}
				msg := err.Error()
				if !strings.Contains(msg, "ns/hooks-webhook") || !strings.Contains(msg, `"`+tt.key+`"`) {
					t.Errorf("%s: the error %q does not name the Secret ns/hooks-webhook and the key %s", what, msg, tt.key)
				}
				if n := strings.Count(strings.ToLower(msg), "connection ns/hooks:"); n != 1 {
					t.Errorf("%s: the error %q names the connection %d times; want once", what, msg, n)
				}
				if tt.value != "" && strings.Contains(msg, strings.TrimSpace(tt.value)) {
					t.Errorf("%s: the error %q holds the credential's value", what, msg)
				}
			}
		})
	}
	if err := cfg.checkOwnConnection(webhookConn("hooks", webhookSpec(), fakeWebhookSecret)); err != nil {
		t.Errorf("checkOwnConnection of a webhook connection the platform takes = %v; want nil", err)
	}
	if err := (RunConfig{}).checkOwnConnection(webhookConn("hooks", webhookSpec(), fakeWebhookSecret)); !errors.Is(err, ErrWiring) ||
		!strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") {
		t.Errorf("checkOwnConnection of a webhook connection with no chat listener = %v; want a wiring error that names AGENTKOURIER_CHAT_LISTEN", err)
	}
}

// The API's spec.webhook maps onto the platform's Config field for field: the scheme's key of the credentials Secret, with the Secret and
// key an error names; the timestamp's unit, s or ms, as a duration; whenMissing keep; id fields; the thread and the delivery header; and the
// API's channel shape.
func TestWebhookConfigMapsTheAPIOntoThePlatform(t *testing.T) {
	cfg, err := webhookConfig(webhookConn("github", webhookSpec(), fakeWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ValidChannel == nil || !cfg.ValidChannel("acme/widgets") || cfg.ValidChannel("general alerts") {
		t.Error("ValidChannel is not the API's webhook channel shape")
	}
	cfg.ValidChannel = nil
	want := webhook.Config{
		Connection: "ns/github", Identity: "acme-review-agent",
		Verify: webhook.Verify{Scheme: webhook.SchemeHMACSHA256, Header: "X-Hub-Signature-256", Prefix: "sha256=",
			Secret:    webhook.Credential{Secret: "ns/github-webhook", Key: v1alpha1.WebhookSecretKey, Value: secret.New(fakeWebhookSecret)},
			Timestamp: &webhook.Timestamp{From: "/ts", Unit: time.Millisecond, Window: time.Minute}},
		Channel:        webhook.Channel{From: []string{"/repository/full_name"}},
		Actor:          webhook.Actor{From: []string{"/sender/login"}, KeepMissing: true},
		Fields:         []webhook.Field{{Name: "number", From: []string{"/number"}, ID: true}, {Name: "event", Header: "X-GitHub-Event"}},
		Thread:         &webhook.Thread{Field: "number", Prefix: "pr-"},
		DeliveryHeader: "X-GitHub-Delivery",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("webhookConfig =\n%+v\nwant\n%+v", cfg, want)
	}

	spec := webhookSpec()
	spec.Verify = v1alpha1.WebhookVerify{Scheme: v1alpha1.WebhookToken, Header: "X-Token", Prefix: "Bearer "}
	spec.Actor = v1alpha1.WebhookActor{None: true}
	spec.Identity, spec.Thread, spec.Delivery = "", nil, nil
	cfg, err = webhookConfig(webhookConn("tok", spec, "fake-token-of-thirty-two-bytes-at-least"))
	if err != nil {
		t.Fatal(err)
	}
	if v := cfg.Verify; v.Scheme != webhook.SchemeToken || v.Secret.Key != v1alpha1.WebhookTokenKey || v.Timestamp != nil || v.Secret.Value.Reveal() != "fake-token-of-thirty-two-bytes-at-least" {
		t.Errorf("the token scheme maps to %+v; want the token key and no timestamp", v)
	}
	if !cfg.Actor.None || cfg.Actor.KeepMissing || cfg.Thread != nil || cfg.DeliveryHeader != "" {
		t.Errorf("actor none, no thread and no delivery map to %+v", cfg)
	}

	seconds := webhookSpec()
	seconds.Verify.Timestamp.Unit = v1alpha1.WebhookUnitSeconds
	if cfg, err := webhookConfig(webhookConn("s", seconds, fakeWebhookSecret)); err != nil || cfg.Verify.Timestamp.Unit != time.Second {
		t.Errorf("a timestamp in s maps to %+v, %v; want a unit of a second", cfg.Verify.Timestamp, err)
	}
}

// A webhook ChatConnection is read with the one key of its scheme, and its Connection carries a copy of its spec.webhook, which shares nothing
// with the snapshot's object, and the credentials Secret its errors name.
func TestAWebhookConnectionIsReadWithTheKeyOfItsScheme(t *testing.T) {
	c := &v1alpha1.ChatConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "github"},
		Spec:       v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWebhook, CredentialsSecretRef: v1alpha1.SecretRef{Name: "github-webhook"}, Webhook: webhookSpec()},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the webhook connection is refused: %v", err)
	}
	r := &reader{vals: map[string]string{"platform/github-webhook/webhookSecret": fakeWebhookSecret, "platform/github-webhook/token": "fake-unread"}}
	conn, err := connectionOf(t.Context(), c, r)
	if err != nil {
		t.Fatalf("connectionOf(webhook) = %v; want the connection", err)
	}
	if len(conn.Credentials) != 1 || conn.Credentials[v1alpha1.WebhookSecretKey].Reveal() != fakeWebhookSecret {
		t.Errorf("the connection has %d credentials; want webhookSecret alone", len(conn.Credentials))
	}
	if conn.Secret != "platform/github-webhook" || conn.Platform != v1alpha1.PlatformWebhook {
		t.Errorf("Secret %q, Platform %q; want platform/github-webhook and webhook", conn.Secret, conn.Platform)
	}
	if !reflect.DeepEqual(conn.Webhook, c.Spec.Webhook) || conn.Webhook == c.Spec.Webhook {
		t.Error("the connection's spec.webhook is not a copy of the ChatConnection's")
	}
	c.Spec.Webhook.Fields[0].From[0] = "/changed"
	if conn.Webhook.Fields[0].From[0] != "/number" {
		t.Error("a change of the snapshot's object reached the connection")
	}
	if _, err := connectionOf(t.Context(), c, &reader{vals: map[string]string{}}); !errors.Is(err, ErrWiring) || !errors.Is(err, secret.ErrEmptyValue) {
		t.Errorf("connectionOf(webhook) with an empty secret = %v; want a wiring error", err)
	}
}

// cloneWebhook shares nothing with what it copies, at any depth: every pointer and slice of a spec.webhook with every field set is a new
// one in the copy, and the copy is equal. The spec is filled by reflection, so a pointer or slice field added to the API and not to
// cloneWebhook fails here.
func TestCloneWebhookSharesNothing(t *testing.T) {
	if cloneWebhook(nil) != nil {
		t.Error("cloneWebhook(nil) is not nil")
	}
	orig := &v1alpha1.WebhookSpec{}
	fill(reflect.ValueOf(orig).Elem())
	c := cloneWebhook(orig)
	if !reflect.DeepEqual(c, orig) {
		t.Fatalf("the copy differs:\n%+v\nwant\n%+v", c, orig)
	}
	shared(t, "WebhookSpec", reflect.ValueOf(orig), reflect.ValueOf(c))
}

// fill sets every field of v, at any depth, to a value that is not its zero: a pointer to a filled value, a slice of two filled elements.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range v.Len() {
			fill(v.Index(i))
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k)
		fill(e)
		v.SetMapIndex(k, e)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	}
}

// shared fails t for every pointer, slice or map of a that b holds too, at any depth.
func shared(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the copy shares the pointer", path)
		}
		shared(t, path, a.Elem(), b.Elem())
	case reflect.Struct:
		for i := range a.NumField() {
			if a.Type().Field(i).IsExported() {
				shared(t, path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
			}
		}
	case reflect.Slice:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			t.Errorf("%s: the copy shares the slice's array", path)
		}
		for i := range a.Len() {
			shared(t, path+"[]", a.Index(i), b.Index(i))
		}
	case reflect.Map:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			t.Errorf("%s: the copy shares the map", path)
		}
	}
}

// The list of connections the verification goes by changes when a webhook connection's spec.webhook does, or its credentials Secret, and
// not when nothing does.
func TestSameConnectionsComparesTheWebhookSpec(t *testing.T) {
	a := []Connection{webhookConn("github", webhookSpec(), fakeWebhookSecret)}
	same := []Connection{webhookConn("github", cloneWebhook(webhookSpec()), fakeWebhookSecret)}
	if !sameConnections(a, same) {
		t.Error("two lists of one connection with equal specs are not the same")
	}
	edited := []Connection{webhookConn("github", webhookSpec(), fakeWebhookSecret)}
	edited[0].Webhook.Fields[0].From = []string{"/pull_request/number"}
	if sameConnections(a, edited) {
		t.Error("a list whose connection's spec.webhook was edited is the same")
	}
	moved := []Connection{webhookConn("github", webhookSpec(), fakeWebhookSecret)}
	moved[0].Secret = "ns/other"
	if sameConnections(a, moved) {
		t.Error("a list whose connection names another Secret is the same")
	}
}

// A broker whose own connection is a webhook connection that its platform cannot be built for refuses to start, with a wiring error that
// names the Secret and key, before it opens the store: a standby would otherwise start and say Ready, and fail only when it took over.
func TestRunRefusesAnOwnWebhookConnectionWithASecretThePlatformRefuses(t *testing.T) {
	c := startConfig(t)
	c.ConfigDir = t.TempDir()
	conn := `apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: {name: github, namespace: hooks}
spec:
  platform: webhook
  credentialsSecretRef: {name: github-webhook}
  webhook:
    identity: acme-review-agent
    verify: {scheme: hmac-sha256, header: X-Hub-Signature-256, prefix: "sha256="}
    channel: {from: [/repository/full_name]}
    actor: {from: [/sender/login]}
`
	if err := os.WriteFile(filepath.Join(c.ConfigDir, "chatconnection.yaml"), []byte(conn), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Client = fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "hooks", Name: "github-webhook"},
		Data: map[string][]byte{v1alpha1.WebhookSecretKey: []byte(fakeWebhookSecret + "\n")}})
	c.ChatListener = someListener(t)
	c.DBPath = unmade(t)
	err := start(t, c).wait()
	if !errors.Is(err, ErrWiring) || !errors.Is(err, webhook.ErrControlCharacter) || !strings.Contains(err.Error(), "hooks/github-webhook") {
		t.Errorf("Run = %v; want a wiring error naming the Secret hooks/github-webhook", err)
	}
	if strings.Contains(err.Error(), fakeWebhookSecret) {
		t.Errorf("the error %q holds the secret", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database exists after the refusal (%v): the check must come before the store", err)
	}
}

// The webhook platform's handler answers as the platform does once it runs: here a delivery with no signature, on the platform newPlatform
// built, is answered 401 and counted once by its reason, on the recorder of the broker's instruments in the context it was built on.
func TestTheWebhookPlatformCountsOnTheBrokersInstruments(t *testing.T) {
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(telemetry.WithObserver(t.Context(), obs))
	defer stop()
	cfg := RunConfig{ChatListener: someListener(t)}
	built, err := cfg.newPlatform(ctx, webhookConn("github", webhookSpec(), fakeWebhookSecret), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- built.Run(ctx, &inboundSink{}) }()
	h, _ := receiverOf(built.Platform)
	testutil.Eventually(t, "the platform to answer an unsigned delivery 401 once it runs", func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/webhook/ns/github", strings.NewReader(`{}`)))
		return rec.Code == http.StatusUnauthorized
	})
	stop()
	if err := <-ran; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v after its context ended; want context.Canceled", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var unsigned, series float64
	for _, f := range families {
		if f.GetName() != "agentkourier_webhook_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			series++
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["outcome"] == "unverified" && labels["reason"] == "unsigned" && labels["connection"] == "ns/github" {
				unsigned = m.GetCounter().GetValue()
			}
		}
	}
	if unsigned != 1 || series < 2 {
		t.Errorf("agentkourier_webhook_requests_total has %v series and {unverified,unsigned,ns/github} = %v; want every series and the unsigned delivery counted once",
			series, unsigned)
	}
}
