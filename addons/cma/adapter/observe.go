package adapter

import (
	"context"
	"errors"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// The adapter's calls to the Managed Agents API, the operation label of agentkourier_cma_operation_duration_seconds: a
// closed set.
const (
	opCreateSession telemetry.Operation = "create_session"
	opStream        telemetry.Operation = "stream"
	opSend          telemetry.Operation = "send"
	opListSessions  telemetry.Operation = "list_sessions"
	opArchive       telemetry.Operation = "archive"
)

// Observe returns b with each call timed (agentkourier_cma_operation_duration_seconds) and traced
// (agentkourier.cma.backend.<operation>, a child of the turn's span). A stream is timed while it opens, not while it
// runs. Nil tel observes nothing.
func Observe(b Backend, tel *telemetry.Telemetry) Backend {
	return observed{next: b, tel: tel}
}

type observed struct {
	next Backend
	tel  *telemetry.Telemetry
}

func (o observed) CreateSession(ctx context.Context, title string, metadata map[string]string) (string, error) {
	return telemetry.Observe(ctx, o.tel, opCreateSession, statusOf, func(ctx context.Context) (string, error) {
		return o.next.CreateSession(ctx, title, metadata)
	})
}

func (o observed) Stream(ctx context.Context, sessionID string) (EventStream, error) {
	// The call's context is ctx with the call's span, so the stream lives as long as ctx; the span ends once it is open.
	return telemetry.Observe(ctx, o.tel, opStream, statusOf, func(ctx context.Context) (EventStream, error) {
		return o.next.Stream(ctx, sessionID)
	})
}

func (o observed) Send(ctx context.Context, sessionID string, events ...Outbound) error {
	_, err := telemetry.Observe(ctx, o.tel, opSend, statusOf, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, o.next.Send(ctx, sessionID, events...)
	})
	return err
}

func (o observed) Sessions(ctx context.Context) ([]SessionInfo, error) {
	return telemetry.Observe(ctx, o.tel, opListSessions, statusOf, func(ctx context.Context) ([]SessionInfo, error) {
		return o.next.Sessions(ctx)
	})
}

func (o observed) Archive(ctx context.Context, sessionID string) error {
	_, err := telemetry.Observe(ctx, o.tel, opArchive, statusOf, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, o.next.Archive(ctx, sessionID)
	})
	return err
}

// statusOf reads the HTTP status the API answered, from the error the SDK returns for one.
func statusOf(err error) (int, bool) {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, true
	}
	return 0, false
}
