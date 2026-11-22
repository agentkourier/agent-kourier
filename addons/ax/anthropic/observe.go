package anthropic

import (
	"context"
	"errors"
	"net"
	"strconv"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"

	"github.com/agentkourier/agent-kourier/addons/ax/telemetry"
)

// Observe returns c with each stream timed while it opens (agentkourier_ax_operation_duration_seconds, operation
// messages_stream) and traced (agentkourier.ax.model.messages_stream, a child of the turn's span): the SDK makes the
// request, retries included, before it returns the stream. Nil tel observes nothing.
func Observe(c Client, tel *telemetry.Telemetry) Client {
	return observed{next: c, tel: tel}
}

type observed struct {
	next Client
	tel  *telemetry.Telemetry
}

func (o observed) NewStreaming(ctx context.Context, params sdk.MessageNewParams, opts ...option.RequestOption) *ssestream.Stream[sdk.MessageStreamEventUnion] {
	// callCtx is ctx with the call's span, so the stream lives as long as ctx; the span ends once it is open.
	callCtx, call := o.tel.StartCall(ctx, telemetry.OpMessagesStream)
	s := o.next.NewStreaming(callCtx, params, opts...)
	call.End(callOutcome(s.Err()))
	return s
}

// callOutcome is the bounded class of a call's error: success, cancelled, timeout, the HTTP status the API answered (an
// error the SDK returns for one), or error. Never the error's text.
func callOutcome(err error) string {
	var apiErr *sdk.Error
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
