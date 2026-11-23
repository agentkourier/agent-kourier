package bedrock

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// Bedrock's calls, the operation label of agentkourier_ax_operation_duration_seconds.
const (
	OpConverse       telemetry.Operation = "converse"
	OpConverseStream telemetry.Operation = "converse_stream"
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
	return telemetry.Observe(ctx, o.tel, OpConverse, statusOf, func(ctx context.Context) (*bedrockruntime.ConverseOutput, error) {
		return o.next.Converse(ctx, in, opts...)
	})
}

func (o observed) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	// The call's context is ctx with the call's span, so the stream lives as long as ctx; the span ends once it is open.
	return telemetry.Observe(ctx, o.tel, OpConverseStream, statusOf, func(ctx context.Context) (*bedrockruntime.ConverseStreamOutput, error) {
		return o.next.ConverseStream(ctx, in, opts...)
	})
}

// statusOf reads the HTTP status Bedrock answered, from an SDK error that carries one.
func statusOf(err error) (int, bool) {
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		return status.HTTPStatusCode(), true
	}
	return 0, false
}
