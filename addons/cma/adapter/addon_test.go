package adapter

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/kittest"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// kitScript is the agent of kitSubject: "hold" runs until the session is interrupted, "clean up" asks to run a tool
// and its approval completes, and anything else completes at once.
func kitScript(session string, out Outbound) []Event {
	switch {
	case out.Message != nil && *out.Message == "hold":
		return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running(), say("holding")}
	case out.Message != nil && *out.Message == "clean up", out.Confirmation != nil:
		return approvalScript(session, out)
	case out.Message != nil:
		return script(say("done"), idle(stopEndTurn))(session, out)
	case out.Interrupt:
		return []Event{idle(stopEndTurn)}
	}
	return nil
}

// kitSubject is the adapter on a fake CMA, as kittest drives it. The replicas of a run share the fake, as they share
// CMA.
func kitSubject() kittest.Subject[*store.Store] {
	fake := newFake(kitScript)
	return kittest.Subject[*store.Store]{
		Addon:    Addon("9.9.9-test", func(config.Config) Backend { return fake }),
		Env:      map[string]string{"CMA_AGENT_ID": "agent_fake", "CMA_ENVIRONMENT_ID": "env_fake"},
		Complete: "hello", Pause: "clean up", Hold: "hold", Answer: "approved",
		AnswerMetadata: map[string]any{HITLExtension: map[string]any{"type": "tool_approval_response",
			"approvals": []any{map[string]any{"id": "sevt_ask", "approved": true}}}},
		BadAnswer: []*a2a.Part{a2a.NewTextPart("yes, go ahead")}, // an approval is structured; text alone is refused
	}
}

// The adapter keeps what the kit's runtime promises, on SQLite and on Postgres.
func TestTheAdapterOnTheKit(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) { kittest.RunSQLite(t, kitSubject()) })
	t.Run("Postgres", func(t *testing.T) { kittest.RunPostgres(t, kitSubject()) })
}

// The names the runtime derives from the add-on's are the ones the adapter has always had: its metrics' and spans'
// prefix, the text of a task a restart cut off, and the header a standby forwards with.
func TestTheAdaptersNames(t *testing.T) {
	a := Addon("9.9.9-test", nil)
	for _, tc := range []struct{ what, got, want string }{
		{"metric prefix", a.TelemetrySpec().Name, "cma"},
		{"interrupted text", runtime.InterruptedMessage(a.Noun), "Interrupted by an adapter restart."},
		{"hop header", cluster.HopHeader(a.Name), "X-Agent-Kourier-Cma-Forwarded"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.what, tc.got, tc.want)
		}
	}
}

// The card names the agent, its version and the tool approval extension, and points at the A2A endpoint.
func TestTheCard(t *testing.T) {
	in := kittest.Start(t, kitSubject(), "cma-0", map[string]string{"SQLITE_PATH": filepath.Join(t.TempDir(), "cma.db")}, runtime.Options{})
	kittest.WaitFor(t, "the adapter to be ready", in.Ready)
	status, body := kittest.Probe(http.MethodGet, in.Base+runtime.A2APath+runtime.CardPath, kittest.Token)
	var card a2a.AgentCard
	if status != http.StatusOK || json.Unmarshal([]byte(body), &card) != nil {
		t.Fatalf("card = %d %s", status, body)
	}
	if card.Name != "claude-managed-agent" || card.Description != "A Claude Managed Agents agent served over A2A" || card.Version != "9.9.9-test" {
		t.Errorf("card = %s %q %s", card.Name, card.Description, card.Version)
	}
	if ext := card.Capabilities.Extensions; len(ext) != 1 || ext[0].URI != HITLExtension {
		t.Errorf("card extensions = %+v, want the tool approval extension", ext)
	}
	if want := in.Base + runtime.A2APath; len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != want {
		t.Errorf("card interfaces = %+v, want %s", card.SupportedInterfaces, want)
	}
}

// A takeover fails and counts the turns a restart cut off, one whose stored record does not decode too, and
// interrupts their session; it archives the sessions a crash left beside a context's stored one as superseded, and
// forgets a context whose session CMA no longer lists as gone. The upkeep then archives the idle session.
func TestATakeoverAndTheUpkeepAreCounted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cma.db")
	clk := newClock()
	st := openStore(t, path, clk.now)
	ctx := kourier(t.Context())
	for _, tk := range []*a2a.Task{
		{ID: "working", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		{ID: "unreadable", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
	} {
		if _, err := st.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	corrupt(t, st, "unreadable")
	for _, s := range []store.Session{{ContextID: "c1", SessionID: "sesn_c1"}, {ContextID: "gone", SessionID: "sesn_gone"}} {
		s.LastActive = clk.now()
		if err := st.PutSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := kitSubject()
	fake := newFake(kitScript)
	fake.seed("sesn_c1", "c1", clk.now())
	fake.seed("sesn_c1_extra", "c1", clk.now().Add(-time.Hour))
	s.Addon = Addon("9.9.9-test", func(config.Config) Backend { return fake })
	in := kittest.Start(t, s, "cma-0", map[string]string{"SQLITE_PATH": path, "SESSION_IDLE_TTL": "1h"},
		runtime.Options{UpkeepInterval: 20 * time.Millisecond, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }})
	kittest.WaitFor(t, "the adapter to be ready", in.Ready)
	for series, want := range map[string]string{
		`agentkourier_cma_tasks_interrupted_total`:                                                       "2",
		`agentkourier_cma_turns_total{outcome="interrupted"}`:                                            "2",
		`agentkourier_cma_sessions_archived_total{reason="superseded"}`:                                  "1",
		`agentkourier_cma_sessions_archived_total{reason="gone"}`:                                        "1",
		`agentkourier_cma_sessions_archived_total{reason="idle"}`:                                        "1",
		`agentkourier_cma_operation_duration_seconds_count{operation="list_sessions",outcome="success"}`: "1",
		`agentkourier_cma_operation_duration_seconds_count{operation="archive",outcome="success"}`:       "2",
		`agentkourier_cma_sessions`:                                                                      "0",
	} {
		in.WantMetric(t, series, want)
	}
	var interrupted []string
	for _, e := range fake.sentEvents() {
		if e.out.Interrupt {
			interrupted = append(interrupted, e.session)
		}
	}
	if len(interrupted) != 1 || interrupted[0] != "sesn_c1" {
		t.Errorf("interrupted %v, want the cut-off turns' session once", interrupted)
	}
}
