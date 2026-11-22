package bedrock

import (
	"context"
	"errors"
	"net"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/agentkourier/agent-kourier/addons/ax/telemetry"
)

// Observe returns c with each call timed (agentkourier_ax_operation_duration_seconds) and traced
// (agentkourier.ax.model.<operation>, a child of the turn's span). A stream is timed while it opens, not while it
// runs. Nil tel observes nothing.
func Observe(c Client, tel *telemetry.Telemetry) Client {
	return observed{next: c, tel: tel}
}

type observed struct {
	next Client
	tel  *telemetry.Telemetry
}

func (o observed) Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	ctx, call := o.tel.StartCall(ctx, telemetry.OpConverse)
	out, err := o.next.Converse(ctx, in, opts...)
	call.End(callOutcome(err))
	return out, err
}

func (o observed) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	// callCtx is ctx with the call's span, so the stream lives as long as ctx; the span ends once it is open.
	callCtx, call := o.tel.StartCall(ctx, telemetry.OpConverseStream)
	out, err := o.next.ConverseStream(callCtx, in, opts...)
	call.End(callOutcome(err))
	return out, err
}

// callOutcome is the bounded class of a call's error: success, cancelled, timeout, the HTTP status Bedrock answered
// (an SDK error that carries one), or error. Never the error's text.
func callOutcome(err error) string {
	var status interface{ HTTPStatusCode() int }
	var netErr net.Error
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &status) && status.HTTPStatusCode() >= 100 && status.HTTPStatusCode() <= 599:
		return strconv.Itoa(status.HTTPStatusCode())
	}
	return "error"
}
