package webhook_test

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// requestSpan is the name of the span of a request to a webhook connection.
const requestSpan = "agentkourier.webhook.request"

// attrsOf returns the attributes of s by key.
func attrsOf(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, a := range s.Attributes {
		out[a.Key] = a.Value
	}
	return out
}

// checkTraced fails t unless the request answered code with series ("outcome reason") ended one span after the first before of tel's:
// a root server span of the connection, with the outcome, the reason and the status code; on a 5xx, the outcome as error.type and as
// an error status, but for cancelled (the sender went away), whose status is unset; the event's ID once the request was verified, and
// no delivery ID, since none of the requests that come here has one.
func checkTraced(t *testing.T, tel *telemetrytest.Harness, series string, code, before int) {
	t.Helper()
	spans := tel.Exporter.GetSpans()[before:]
	if len(spans) != 1 {
		t.Errorf("%s: %d spans ended, want 1", series, len(spans))
		return
	}
	s := spans[0]
	outcome, reason, _ := strings.Cut(series, " ")
	if s.Name != requestSpan || s.SpanKind != trace.SpanKindServer || s.Parent.IsValid() {
		t.Errorf("%s: span %s of kind %s under %v; want a root %s of kind server", series, s.Name, s.SpanKind, s.Parent.SpanID(), requestSpan)
	}
	attrs := attrsOf(s)
	for k, want := range map[attribute.Key]string{"agentkourier.platform": "webhook", "agentkourier.connection": "ns/hooks",
		"agentkourier.outcome": outcome, "agentkourier.webhook.reason": reason} {
		if got := attrs[k].AsString(); got != want {
			t.Errorf("%s: %s = %q, want %q", series, k, got, want)
		}
	}
	if got := attrs["http.response.status_code"].AsInt64(); got != int64(code) {
		t.Errorf("%s: http.response.status_code = %d, want %d", series, got, code)
	}
	wantType, wantStatus := "", codes.Unset
	if code >= http.StatusInternalServerError {
		wantType = outcome
		if outcome != "cancelled" {
			wantStatus = codes.Error
		}
	}
	if got := attrs["error.type"].AsString(); got != wantType {
		t.Errorf("%s: error.type = %q, want %q", series, got, wantType)
	}
	if s.Status.Code != wantStatus || (wantStatus == codes.Error && s.Status.Description != outcome) {
		t.Errorf("%s: status %s %q, want %s", series, s.Status.Code, s.Status.Description, wantStatus)
	}
	verified := outcome != "unverified" && series != "malformed unreadable"
	if ev, ok := attrs["agentkourier.webhook.event"]; ok != verified || (ok && len(ev.AsString()) != 64) {
		t.Errorf("%s: agentkourier.webhook.event = %q (set %v); want the body's SHA-256 if and only if the request was verified", series,
			ev.AsString(), ok)
	}
	if _, ok := attrs["agentkourier.webhook.delivery"]; ok {
		t.Errorf("%s: a delivery ID is on the span of a request that has none", series)
	}
}

// A request's span is the parent of what the Sink does with its event: the Sink is called on a context of Run's (its values) that
// carries the request's span, still open, so that the intake's span is its child. The request's log lines carry its trace: the INFO
// line, and the DEBUG and WARN lines of an unverified one. The sender's delivery ID is on the span when it is an identifier, and the
// event's ID always once the request is verified.
func TestARequestSpanIsTheParentOfTheSinksWorkAndOfItsLogLines(t *testing.T) {
	var buf syncBuffer
	tel := telemetrytest.New(t)
	cfg := config(nil, slog.New(telemetry.LogHandler{Handler: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})}))
	cfg.Tracer = telemetry.Tracer(tel.Ctx)
	p := newPlatform(t, cfg)
	sk := &sink{}
	var open bool // whether the span in the Sink's context was recording when the Sink was called
	sk.hook = func() {
		sk.mu.Lock()
		defer sk.mu.Unlock()
		open = trace.SpanFromContext(sk.ctxs[len(sk.ctxs)-1]).IsRecording()
	}
	running(t, p, sk)

	const delivery = "72d3162e-cc78-11e3-81ab-4c9367dc0958"
	ok := request(event, true)
	ok.Header.Set("X-GitHub-Delivery", delivery)
	if w := serve(t, p, ok); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	spans := tel.Exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want the request's", len(spans))
	}
	accepted := spans[0]
	sum := sha256.Sum256([]byte(event))
	attrs := attrsOf(accepted)
	if attrs["agentkourier.webhook.event"].AsString() != hex.EncodeToString(sum[:]) || attrs["agentkourier.webhook.delivery"].AsString() != delivery {
		t.Errorf("the accepted span's event and delivery are %q and %q; want the body's SHA-256 and %q",
			attrs["agentkourier.webhook.event"].AsString(), attrs["agentkourier.webhook.delivery"].AsString(), delivery)
	}
	sk.mu.Lock()
	ctx := sk.ctxs[0]
	sk.mu.Unlock()
	if got := trace.SpanContextFromContext(ctx); got.SpanID() != accepted.SpanContext.SpanID() || !open {
		t.Errorf("the Sink was called under span %s (recording %v); want the request's, %s, still open", got.SpanID(), open,
			accepted.SpanContext.SpanID())
	}
	if ctx.Value(ctxKey{}) != "run" {
		t.Error("the Sink's context lacks Run's values")
	}

	bad := request(`{"sender":{"login":"octocat"}}`, true)
	bad.Header.Set("X-GitHub-Delivery", "not an id <script>")
	serve(t, p, bad)
	unmapped := tel.Exporter.GetSpans()[1]
	if _, ok := attrsOf(unmapped)["agentkourier.webhook.delivery"]; ok {
		t.Error("a delivery ID that is not an identifier is on the span")
	}

	forged := request(event, false)
	forged.Header.Set(sigHeader, "sha256=zq-forged-signature")
	serve(t, p, forged)
	unverified := tel.Exporter.GetSpans()[2]

	lines := logLines(t, buf.String())
	for _, c := range []struct {
		msg  string
		span tracetest.SpanStub
		n    int
	}{
		{"webhook: delivery answered", accepted, 0},
		{"webhook: delivery answered", unmapped, 1},
		{"webhook: a request was refused: it is not verified", unverified, 0},
		{"webhook: requests were refused: they are not verified", unverified, 0},
	} {
		got := linesOf(lines, c.msg)
		if len(got) <= c.n {
			t.Errorf("no line %d says %q:\n%s", c.n, c.msg, buf.String())
			continue
		}
		if l := got[c.n]; l["trace_id"] != c.span.SpanContext.TraceID().String() || l["span_id"] != c.span.SpanContext.SpanID().String() {
			t.Errorf("the line %q has trace_id %v and span_id %v; want its request's, %s and %s", c.msg, l["trace_id"], l["span_id"],
				c.span.SpanContext.TraceID(), c.span.SpanContext.SpanID())
		}
	}
	// Nothing of what a request held but the delivery ID that passed reaches a span: its body, its signature, a delivery ID that is
	// not an identifier, the secret.
	tel.RequireNone(t, "Ignore previous instructions", "octocat", sign(fakeSecret, event), "zq-forged-signature", "not an id", fakeSecret)
}
