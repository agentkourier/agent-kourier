package telemetry

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Transport traces HTTP requests, excluding URLs, headers and bodies. propagate is true
// only for the verifying A2A front door; Slack and other third parties receive no trace headers.
func Transport(next http.RoundTripper, propagate bool) http.RoundTripper {
	return tracedTransport{next: next, propagate: propagate}
}

type tracedTransport struct {
	next      http.RoundTripper
	propagate bool
}

func (t tracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, op := StartKind(req.Context(), "agentkourier.http.client", trace.SpanKindClient,
		attribute.String("http.request.method", req.Method), attribute.String("server.address", req.URL.Hostname()))
	op.httpMethod, op.httpServer = req.Method, req.URL.Hostname()
	copyReq := req.Clone(ctx)
	copyReq.Header.Del("baggage")
	copyReq.Header.Del("traceparent")
	copyReq.Header.Del("tracestate")
	if port, err := strconv.Atoi(req.URL.Port()); err == nil {
		trace.SpanFromContext(ctx).SetAttributes(attribute.Int("server.port", port))
	}
	if t.propagate {
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(copyReq.Header))
	}
	resp, err := t.next.RoundTrip(copyReq)
	if err != nil {
		op.Finish(err)
		return resp, err
	}
	op.httpStatus = resp.StatusCode
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	var statusErr error
	if resp.StatusCode >= 400 {
		statusErr = httpStatusError(resp.StatusCode)
	}
	if resp.Body == nil {
		op.Finish(statusErr)
	} else {
		resp.Body = &tracedBody{ReadCloser: resp.Body, op: op, statusErr: statusErr, ctx: ctx}
	}
	return resp, nil
}

type tracedBody struct {
	io.ReadCloser
	op        *Operation
	statusErr error
	ctx       context.Context
}

func (b *tracedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if err == io.EOF {
			b.op.Finish(b.statusErr)
		} else {
			b.op.Finish(err)
		}
	}
	return n, err
}
func (b *tracedBody) Close() error {
	err := b.ReadCloser.Close()
	out := b.statusErr
	if err != nil {
		out = err
	} else if b.ctx.Err() != nil {
		out = b.ctx.Err()
	}
	b.op.Finish(out)
	return err
}

type httpStatusError int

func (e httpStatusError) Error() string { return "HTTP " + strconv.Itoa(int(e)) }
