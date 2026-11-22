package adapter

import (
	"context"
	"errors"
	"net"
	"strconv"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/agentkourier/agent-kourier/addons/cma/telemetry"
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
	ctx, call := o.tel.StartCall(ctx, telemetry.OpCreateSession)
	id, err := o.next.CreateSession(ctx, title, metadata)
	call.End(callOutcome(err))
	return id, err
}

func (o observed) Stream(ctx context.Context, sessionID string) (EventStream, error) {
	callCtx, call := o.tel.StartCall(ctx, telemetry.OpStream)
	// callCtx is ctx with the call's span, so the stream lives as long as ctx; the span ends once it is open.
	s, err := o.next.Stream(callCtx, sessionID)
	call.End(callOutcome(err))
	return s, err
}

func (o observed) Send(ctx context.Context, sessionID string, events ...Outbound) error {
	ctx, call := o.tel.StartCall(ctx, telemetry.OpSend)
	err := o.next.Send(ctx, sessionID, events...)
	call.End(callOutcome(err))
	return err
}

func (o observed) Sessions(ctx context.Context) ([]SessionInfo, error) {
	ctx, call := o.tel.StartCall(ctx, telemetry.OpListSessions)
	out, err := o.next.Sessions(ctx)
	call.End(callOutcome(err))
	return out, err
}

func (o observed) Archive(ctx context.Context, sessionID string) error {
	ctx, call := o.tel.StartCall(ctx, telemetry.OpArchive)
	err := o.next.Archive(ctx, sessionID)
	call.End(callOutcome(err))
	return err
}

// callOutcome is the bounded class of a call's error: success, cancelled, timeout, the HTTP status the API answered
// (an error the SDK returns for one), or error. Never the error's text.
func callOutcome(err error) string {
	var apiErr *anthropic.Error
	var netErr net.Error
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &apiErr) && apiErr.StatusCode >= 100 && apiErr.StatusCode <= 599:
		return strconv.Itoa(apiErr.StatusCode)
	}
	return "error"
}
