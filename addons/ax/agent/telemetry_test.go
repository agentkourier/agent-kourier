package agent

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/addons/ax/bedrock"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// startObserved is start with tel, and the handler behind telemetry.Extract, as the agent serves it.
func startObserved(t *testing.T, m model.LLM, tel *telemetry.Telemetry) *agentAt {
	t.Helper()
	st, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "ax.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Model: m, Store: st, Check: &auth.Check{Verifier: bearerIsCaller{}, Bearer: true, Log: discard()},
		PublicURL: "http://ax.agents.svc:8090/a2a", Version: "1.2.3-test", Telemetry: tel})
	if err != nil {
		t.Fatal(err)
	}
	a := &agentAt{store: st, http: httptest.NewServer(telemetry.Extract(s.Handler))}
	a.client = connect(t, a.http, "binding")
	t.Cleanup(a.stop)
	return a
}

func newTelemetry(t *testing.T, provider trace.TracerProvider) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(TelemetrySpec(), provider)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

// series returns the value of one series, as /metrics prints it, or "" when it is not served.
func series(t *testing.T, tel *telemetry.Telemetry, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			return v
		}
	}
	return ""
}

// wantSeries waits for each series to have its value: a turn is counted when its Execute returns, which can be after
// the caller has read the turn's last event.
func wantSeries(t *testing.T, tel *telemetry.Telemetry, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for name, v := range want {
		got := series(t, tel, name)
		for got != v && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			got = series(t, tel, name)
		}
		if got != v {
			t.Errorf("%s = %q, want %q", name, got, v)
		}
	}
}

// failing is a model whose every call fails.
type failing struct{}

func (failing) Name() string { return "failing" }
func (failing) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(nil, errors.New("bedrock is down")) }
}

// Each turn is counted under how it ended: one that completed, one that paused on a question, its answer, which
// completed, and a resent message, which joined its task and ran nothing.
func TestTurnsAndReplaysAreCounted(t *testing.T) {
	tel := newTelemetry(t, nil)
	a := startObserved(t, &fakeModel{}, tel)
	if task := send(t, a.client, message("m1", "hello")); task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("hello: %+v", task.Status)
	}
	asked := send(t, a.client, message("m2", "a question"))
	if asked.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("question: %+v", asked.Status)
	}
	answer := message("m3", "checkout")
	answer.TaskID, answer.ContextID = asked.ID, asked.ContextID
	if task := send(t, a.client, answer); task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("answer: %+v", task.Status)
	}
	send(t, a.client, message("m1", "hello"))
	wantSeries(t, tel, map[string]string{
		`agentkourier_ax_turns_total{outcome="completed"}`:      "2",
		`agentkourier_ax_turns_total{outcome="input_required"}`: "1",
		`agentkourier_ax_message_replays_total`:                 "1",
	})
}

// A turn whose model call fails is counted as failed.
func TestAFailedTurnIsCounted(t *testing.T) {
	tel := newTelemetry(t, nil)
	a := startObserved(t, failing{}, tel)
	for _, err := range a.client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: message("m1", "hello")}) {
		if err != nil {
			break // the failed turn's error
		}
	}
	wantSeries(t, tel, map[string]string{`agentkourier_ax_turns_total{outcome="failed"}`: "1"})
	if got := series(t, tel, `agentkourier_ax_turns_total{outcome="completed"}`); got != "" {
		t.Errorf("a failed turn was also counted as completed: %s", got)
	}
}

// tracing is a model that starts a span of its own in the context ADK gives it, as Observe does for a Bedrock call.
type tracing struct{ tel *telemetry.Telemetry }

func (tracing) Name() string { return "tracing" }
func (m tracing) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	_, call := m.tel.StartCall(ctx, bedrock.OpConverseStream)
	call.End("success")
	return (&fakeModel{}).GenerateContent(ctx, req, stream)
}

// traceparent sets the caller's trace context on each request, as Agent Kourier does.
type traceparent struct{ base http.RoundTripper }

const callerTrace = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func (tp traceparent) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("traceparent", callerTrace)
	return tp.base.RoundTrip(r)
}

// A turn's span is a child of the caller's (its traceparent), and the model's calls are children of the turn's.
func TestTheTurnIsAChildOfTheCallerAndTheModelOfTheTurn(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	a := startObserved(t, tracing{tel: tel}, tel)
	card := &a2a.AgentCard{Name: "test", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(a.http.URL+"/a2a", a2a.TransportProtocolJSONRPC)}}
	client, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: traceparent{authTransport{a.http.Client().Transport, "binding"}}}))
	if err != nil {
		t.Fatal(err)
	}
	task := send(t, client, message("m1", "hello"))
	var turn, call sdktrace.ReadOnlySpan
	deadline := time.Now().Add(5 * time.Second)
	for (turn == nil || call == nil) && time.Now().Before(deadline) {
		for _, s := range rec.Ended() {
			switch s.Name() {
			case "agentkourier.ax.turn":
				turn = s
			case "agentkourier.ax.model.converse_stream":
				call = s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if turn == nil || call == nil {
		t.Fatalf("spans: turn %v, call %v", turn, call)
	}
	if turn.Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || turn.Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("turn parent = %+v, want the caller's span", turn.Parent())
	}
	if call.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Fatalf("the model's call is not the turn's child: %+v", call.Parent())
	}
	attrs := map[string]string{}
	for _, kv := range turn.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs[telemetry.AttrTaskID] != string(task.ID) || attrs[telemetry.AttrContextID] != task.ContextID || attrs[telemetry.AttrOutcome] != "completed" {
		t.Fatalf("turn attributes = %v", attrs)
	}
}

// What a sweep deletes for idleness is counted, by reason.
func TestUpkeepCountsDeletedSessions(t *testing.T) {
	st, clk := upkeepFixture(t)
	tel := newTelemetry(t, nil)
	metrics, err := NewMetrics(tel)
	if err != nil {
		t.Fatal(err)
	}
	(&Upkeep{Store: st, SessionIdleTTL: 30 * time.Minute, Log: discard(), Now: clk.now, Metrics: metrics}).Sweep(t.Context())
	wantSeries(t, tel, map[string]string{`agentkourier_ax_sessions_deleted_total{reason="idle"}`: "1"})
}
