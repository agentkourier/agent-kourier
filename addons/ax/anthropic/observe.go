package anthropic

import (
	"context"
	"errors"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// OpMessagesStream is the Anthropic API's call, the operation label of agentkourier_ax_operation_duration_seconds: the
// model always streams there.
const OpMessagesStream telemetry.Operation = "messages_stream"

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
	// The call's context is ctx with the call's span, so the stream lives as long as ctx; the span ends once it is open.
	s, _ := telemetry.Observe(ctx, o.tel, OpMessagesStream, statusOf, func(ctx context.Context) (*ssestream.Stream[sdk.MessageStreamEventUnion], error) {
		s := o.next.NewStreaming(ctx, params, opts...)
		return s, s.Err()
	})
	return s
}

// statusOf reads the HTTP status the API answered, from the error the SDK returns for one.
func statusOf(err error) (int, bool) {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, true
	}
	return 0, false
}
