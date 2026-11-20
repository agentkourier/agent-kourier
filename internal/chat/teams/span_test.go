package teams

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// traced makes a receiverTest whose Receiver traces with tel's tracer and whose Serve runs on tel's context (the broker's values), with
// each of opts applied to its config. It does not serve.
func traced(t *testing.T, tel *telemetrytest.Harness, invokes InvokeHandler, opts ...func(*ReceiverConfig)) *receiverTest {
	t.Helper()
	opts = append([]func(*ReceiverConfig){func(c *ReceiverConfig) { c.Tracer = telemetry.Tracer(tel.Ctx) }}, opts...)
	rt := newReceiverTest(t, false, invokes, opts...)
	rt.base = tel.Ctx
	return rt
}

// spanAttrs returns the attributes of s by key.
func spanAttrs(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, a := range s.Attributes {
		out[a.Key] = a.Value
	}
	return out
}

// checkRequestSpan fails t unless s is a root server span of a request to the test connection that answered code with outcome, for an
// activity of the type typ with the ID id; on a 5xx, with the outcome as error.type and as an error status.
func checkRequestSpan(t *testing.T, s tracetest.SpanStub, outcome string, code int, typ, id string) {
	t.Helper()
	if s.Name != requestSpan || s.SpanKind != trace.SpanKindServer || s.Parent.IsValid() {
		t.Errorf("%s: span %s of kind %s under %v; want a root %s of kind server", outcome, s.Name, s.SpanKind, s.Parent.SpanID(), requestSpan)
	}
	attrs := spanAttrs(s)
	for k, want := range map[attribute.Key]string{"agentkourier.platform": "teams", "agentkourier.connection": testConn,
		"agentkourier.outcome": outcome, "agentkourier.teams.activity.type": typ, "agentkourier.teams.activity.id": id} {
		if got := attrs[k].AsString(); got != want {
			t.Errorf("%s: %s = %q, want %q", outcome, k, got, want)
		}
	}
	if got := attrs["http.response.status_code"].AsInt64(); got != int64(code) {
		t.Errorf("%s: http.response.status_code = %d, want %d", outcome, got, code)
	}
	wantType, wantStatus := "", codes.Unset
	if code >= http.StatusInternalServerError {
		wantType, wantStatus = outcome, codes.Error
	}
	if got := attrs["error.type"].AsString(); got != wantType {
		t.Errorf("%s: error.type = %q, want %q", outcome, got, wantType)
	}
	if s.Status.Code != wantStatus || (wantStatus == codes.Error && s.Status.Description != outcome) {
		t.Errorf("%s: status %s %q, want %s", outcome, s.Status.Code, s.Status.Description, wantStatus)
	}
}

// logLine is one JSON line of a log.
type logLine map[string]any

// linesSaying returns the lines of the JSON log text whose msg is msg.
func linesSaying(t *testing.T, text, msg string) []logLine {
	t.Helper()
	var out []logLine
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if l == "" {
			continue
		}
		var line logLine
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("a log line is not JSON: %q", l)
		}
		if line["msg"] == msg {
			out = append(out, line)
		}
	}
	return out
}

// A verified message is one server span, from when the handler was called to the answer, and the parent of what the sink does with its
// post: the lane calls the sink on Serve's context (the broker's values) under the request's span context, though the span ended with
// the answer. The lane's own lines carry its trace. Nothing of the activity but its type and ID, and nothing of the token, reaches it.
func TestAMessagesRequestSpanIsTheParentOfTheSinksWork(t *testing.T) {
	tel := telemetrytest.New(t)
	var logs syncBuffer
	// The clock is first read in a request to verify its token: when it is, a millisecond on, is after the handler was called.
	var armed atomic.Bool
	var verifiedAt atomic.Pointer[time.Time]
	rt := traced(t, tel, nil, func(c *ReceiverConfig) {
		c.Logger = slog.New(telemetry.LogHandler{Handler: slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})})
		inner := c.Now
		c.Now = func() time.Time {
			if armed.Load() && verifiedAt.Load() == nil {
				time.Sleep(time.Millisecond)
				now := time.Now()
				verifiedAt.CompareAndSwap(nil, &now)
			}
			return inner()
		}
	})
	rt.serve(t)
	testutil.Eventually(t, "the keys are fetched", func() bool {
		rt.r.verifier.mu.Lock()
		defer rt.r.verifier.mu.Unlock()
		return rt.r.verifier.keys != nil
	})
	authz := bearer(t, rt.key, claimsAt(rt.clock.now()))
	armed.Store(true)
	if rec := rt.post(t, fixture(t, "message_mention"), authz); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	testutil.Eventually(t, "the sink has the message", func() bool { return rt.sink.has("1794965322719") })
	req := tel.Span(t, requestSpan)
	checkRequestSpan(t, req, requestAccepted, http.StatusOK, "message", "1794965322719")
	if at := verifiedAt.Load(); at == nil || !req.StartTime.Before(*at) {
		t.Errorf("the span starts at %v, and the token was verified at %v; want the span timed from before the verify", req.StartTime, at)
	}
	rt.sink.mu.Lock()
	ctx := rt.sink.ctxs[0]
	rt.sink.mu.Unlock()
	if got := trace.SpanContextFromContext(ctx); !got.Equal(req.SpanContext) {
		t.Errorf("the sink was called under span %s of trace %s; want the request's, %s of %s", got.SpanID(), got.TraceID(),
			req.SpanContext.SpanID(), req.SpanContext.TraceID())
	}
	if !telemetry.Enabled(ctx) {
		t.Error("the sink's context lacks Serve's values, the broker's instruments")
	}
	// A child of the sink's context is a child of the request's span, in its trace: as the intake's span is.
	_, child := telemetry.Tracer(tel.Ctx).Start(ctx, "agentkourier.intake")
	child.End()
	if in := tel.Span(t, "agentkourier.intake"); in.Parent.SpanID() != req.SpanContext.SpanID() || in.SpanContext.TraceID() != req.SpanContext.TraceID() {
		t.Errorf("a span started on the sink's context is under %s of trace %s; want the request's span", in.Parent.SpanID(), in.SpanContext.TraceID())
	}

	// The lane's WARN of a post the sink never takes carries the trace of the request that took it.
	const dropped = "1794965000099"
	rt.sink.fail(dropped, failAlways)
	if rec := rt.post(t, inChannel(t, "message_plain", testChannel, dropped), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	const drop = "teams: the sink did not take a message, which is dropped"
	testutil.Eventually(t, "the lane drops the post", func() bool { return len(linesSaying(t, logs.String(), drop)) == 1 })
	var second tracetest.SpanStub
	for _, s := range tel.Exporter.GetSpans() {
		if s.Name == requestSpan && spanAttrs(s)["agentkourier.teams.activity.id"].AsString() == dropped {
			second = s
		}
	}
	line := linesSaying(t, logs.String(), drop)[0]
	if line["trace_id"] != second.SpanContext.TraceID().String() || line["span_id"] != second.SpanContext.SpanID().String() {
		t.Errorf("the drop's line has trace_id %v and span_id %v; want its request's, %s and %s", line["trace_id"], line["span_id"],
			second.SpanContext.TraceID(), second.SpanContext.SpanID())
	}
	tel.RequireNone(t, "echo two-socket test", "Test User", authz)
}

// spanInvokes records the context of each invoke as it is answered: its span context, whether the span was recording, whether it held
// Serve's values, and its deadline. With wait set, it answers once its context ends.
type spanInvokes struct {
	mu        sync.Mutex
	spans     []trace.SpanContext
	recording []bool
	enabled   []bool
	deadlines []time.Time
	wait      bool
	ended     chan struct{} // closed once an invoke's context has ended, with wait
}

func (h *spanInvokes) Invoke(ctx context.Context, _ chat.Sink, _ Invoke) InvokeAnswer {
	h.mu.Lock()
	d, _ := ctx.Deadline()
	h.spans, h.recording = append(h.spans, trace.SpanContextFromContext(ctx)), append(h.recording, trace.SpanFromContext(ctx).IsRecording())
	h.enabled, h.deadlines = append(h.enabled, telemetry.Enabled(ctx)), append(h.deadlines, d)
	wait := h.wait
	h.mu.Unlock()
	if wait {
		<-ctx.Done()
		close(h.ended)
	}
	return InvokeAnswer{Status: http.StatusOK}
}

// An invoke is answered in its request, under its span: the InvokeHandler is called with the span still open, with Serve's values (the
// broker's instruments) so that what it hands the sink is in the trace and counted, and with the request's deadline and end.
func TestAnInvokeIsAnsweredUnderItsRequestSpan(t *testing.T) {
	tel := telemetrytest.New(t)
	h := &spanInvokes{}
	rt := traced(t, tel, h)
	rt.serve(t)
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	if rec := rt.postIn(ctx, t, fixture(t, "invoke_action"), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	req := tel.Span(t, requestSpan)
	checkRequestSpan(t, req, requestInvoke, http.StatusOK, "invoke", "f:00000000-0000-4000-8000-0000000000c3")
	h.mu.Lock()
	if len(h.spans) != 1 || !h.spans[0].Equal(req.SpanContext) || !h.recording[0] || !h.enabled[0] || !h.deadlines[0].Equal(deadline) {
		t.Errorf("the handler was called under %v (recording %v), with the broker's values %v and the deadline %v; want the request's span, open, "+
			"the values and %v", h.spans, h.recording, h.enabled, h.deadlines, deadline)
	}
	h.mu.Unlock()

	// The request's end ends the handler's context, though its values are Serve's.
	h.wait, h.ended = true, make(chan struct{})
	gone, hangUp := context.WithCancel(t.Context())
	body, authz := editFixture(t, "invoke_action", func(m map[string]any) { m["id"] = "f:another-press" }), bearer(t, rt.key, claimsAt(rt.clock.now()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.postIn(gone, t, body, authz)
	}()
	testutil.Eventually(t, "the second invoke reaches the handler", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.spans) == 2
	})
	hangUp()
	select {
	case <-h.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler's context did not end with its request's")
	}
	<-done
}

// A request refused at its token (401, or 503 when the keys cannot be read), whose body is no activity (400), or whose activity is of
// another tenant (403), has no span: anyone who can reach the route can send the first three, and anyone whose tenant installs the bot
// the last. Nor does it end or mark a span that its context already holds.
func TestARefusedRequestHasNoSpan(t *testing.T) {
	tel := telemetrytest.New(t)
	rt := traced(t, tel, nil)
	rt.serve(t)
	keyless := traced(t, tel, nil)
	keyless.f.setStatus(http.StatusInternalServerError)
	keyless.serve(t)
	ctx, outer := telemetry.Tracer(tel.Ctx).Start(t.Context(), "agentkourier.test.root")
	mismatch := claimsAt(rt.clock.now())
	mismatch["serviceurl"] = "https://smba.trafficmanager.net/emea/"
	other := editFixture(t, "message_mention", func(m map[string]any) {
		m["channelData"].(map[string]any)["tenant"] = map[string]any{"id": "00000000-0000-4000-8000-0000000000aa"}
	})
	for _, c := range []struct {
		name  string
		rt    *receiverTest
		body  []byte
		authz string
		code  int
	}{
		{"no token", rt, fixture(t, "message_mention"), "none", http.StatusUnauthorized},
		{"an impostor's key", rt, fixture(t, "message_mention"), bearer(t, newSigningKey(t, "k", "msteams"), claimsAt(rt.clock.now())), http.StatusUnauthorized},
		{"a service URL that is not the activity's", rt, fixture(t, "message_mention"), bearer(t, rt.key, mismatch), http.StatusUnauthorized},
		{"keys that cannot be read", keyless, fixture(t, "message_mention"), "", http.StatusServiceUnavailable},
		{"no activity", rt, []byte(`{"text":"not an activity"}`), "", http.StatusBadRequest},
		{"another tenant", rt, other, "", http.StatusForbidden},
	} {
		if rec := c.rt.postIn(ctx, t, c.body, c.authz); rec.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.code)
		}
	}
	if !outer.IsRecording() {
		t.Error("the context's span was ended by the handler")
	}
	outer.End()
	spans := tel.Exporter.GetSpans()
	if len(spans) != 1 || len(spans[0].Attributes) != 0 || spans[0].Status.Code != codes.Unset {
		t.Errorf("spans %+v; want only the context's, with no attribute or status of the handler's", spans)
	}
}

// A verified request that is not taken has its span all the same, with its outcome and status: 503 not_running and shed are errors, and
// a duplicate and a message refused for its ID are not.
func TestARequestSpanCarriesItsOutcome(t *testing.T) {
	t.Run("not running", func(t *testing.T) {
		tel := telemetrytest.New(t)
		rt := traced(t, tel, nil)
		if rec := rt.post(t, fixture(t, "message_mention"), ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", rec.Code)
		}
		checkRequestSpan(t, tel.Span(t, requestSpan), requestNotRunning, http.StatusServiceUnavailable, "message", "1794965322719")
	})
	t.Run("shed", func(t *testing.T) {
		tel := telemetrytest.New(t)
		rt := traced(t, tel, nil)
		rt.sink.block = make(chan struct{})
		rt.serve(t)
		next := rt.fillLane(t, testChannel, 1794960000000)
		before := len(tel.Exporter.GetSpans())
		if rec := rt.post(t, inChannel(t, "message_plain", testChannel, "1794969999999"), ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("past the lane (next %d): status %d, want 503", next, rec.Code)
		}
		spans := tel.Exporter.GetSpans()[before:]
		if len(spans) != 1 {
			t.Fatalf("%d spans for the shed request, want 1", len(spans))
		}
		checkRequestSpan(t, spans[0], requestShed, http.StatusServiceUnavailable, "message", "1794969999999")
	})
	t.Run("duplicate and malformed", func(t *testing.T) {
		tel := telemetrytest.New(t)
		rt := traced(t, tel, nil)
		rt.serve(t)
		rt.post(t, fixture(t, "message_plain"), "")
		if rec := rt.post(t, fixture(t, "message_plain"), ""); rec.Code != http.StatusOK {
			t.Fatalf("the duplicate: status %d, want 200", rec.Code)
		}
		bad := editFixture(t, "message_plain", func(m map[string]any) { m["id"] = "17949:65057510" })
		if rec := rt.post(t, bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("a message whose ID holds ':': status %d, want 400", rec.Code)
		}
		spans := tel.Exporter.GetSpans()
		if len(spans) != 3 {
			t.Fatalf("%d spans, want 3", len(spans))
		}
		id := spanAttrs(spans[0])["agentkourier.teams.activity.id"].AsString()
		checkRequestSpan(t, spans[1], requestDuplicate, http.StatusOK, "message", id)
		checkRequestSpan(t, spans[2], requestMalformed, http.StatusBadRequest, "message", "17949:65057510")
	})
}

// The Client hands its Config's Tracer to its receiver, and with none the receiver traces with a tracer that records nothing.
func TestNewHandsTheTracerToTheReceiver(t *testing.T) {
	tel := telemetrytest.New(t)
	tr := telemetry.Tracer(tel.Ctx)
	cfg := Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret), Tracer: tr}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.recv.tracer != tr {
		t.Errorf("the receiver's tracer is %v, want the Config's", c.recv.tracer)
	}
	cfg.Tracer = nil
	if c, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	if _, span := c.recv.tracer.Start(t.Context(), requestSpan); span.IsRecording() {
		t.Error("with no Tracer, the receiver's tracer records")
	}
}
