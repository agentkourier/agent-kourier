package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/kittest"
	"github.com/agentkourier/agent-kourier/addons/kit/kittest/echo"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// kitModel answers the messages of kitSubject: "hold" runs until its context ends, a message that names a question
// asks one with ask_user, an answer to it completes, and anything else completes at once.
type kitModel struct{}

func (kitModel) Name() string { return "fake-model" }

func (kitModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		reply := func(parts ...*genai.Part) {
			yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: parts}, TurnComplete: true}, nil)
		}
		for _, p := range req.Contents[len(req.Contents)-1].Parts {
			switch {
			case p.FunctionResponse != nil:
				reply(&genai.Part{Text: "answered"})
				return
			case p.Text == "hold":
				<-ctx.Done()
				yield(nil, ctx.Err())
				return
			case strings.Contains(p.Text, "question"):
				reply(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "ask_user", Args: map[string]any{"question": "Which service?"}}})
				return
			}
		}
		reply(&genai.Part{Text: "done"})
	}
}

// kitSubject is the agent on kitModel, as kittest drives it, with the settings the agent needs beside the kit's.
func kitSubject() kittest.Subject[*store.Store] {
	return kittest.Subject[*store.Store]{
		Addon:    Addon("9.9.9-test", func(context.Context, config.Config, *telemetry.Telemetry) (model.LLM, error) { return kitModel{}, nil }),
		Env:      map[string]string{"MODEL_REGION": "us-east-1", "MODEL_NAME": "fake-model"},
		Complete: "hello", Pause: "ask a question", Hold: "hold", Answer: "checkout",
		BadAnswer: []*a2a.Part{a2a.NewDataPart(map[string]any{"not": "text"})},
	}
}

// The agent keeps what the kit's runtime promises, on SQLite and on Postgres.
func TestTheAgentOnTheKit(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) { kittest.RunSQLite(t, kitSubject()) })
	t.Run("Postgres", func(t *testing.T) { kittest.RunPostgres(t, kitSubject()) })
}

// The kit's reference add-on, on SQLite under the driver the agent links (the kit links none).
func TestTheEchoAddonOnSQLite(t *testing.T) { kittest.RunSQLite(t, echo.Subject()) }

func sqliteEnv(t *testing.T) map[string]string {
	return map[string]string{"SQLITE_PATH": filepath.Join(t.TempDir(), "state", "ax.db")}
}

// Standalone, the agent answers its own readiness and has no metadata proxy; the card names the version and
// PUBLIC_URL's default.
func TestStandalone(t *testing.T) {
	s := kitSubject()
	in := kittest.Start(t, s, "ax-0", sqliteEnv(t), runtime.Options{})
	kittest.WaitFor(t, "the agent to be ready", in.Ready)
	if got, _ := kittest.Probe(http.MethodGet, in.Base+"/metadata/x", ""); got != http.StatusNotFound {
		t.Errorf("/metadata/x = %d, want 404", got)
	}
	status, body := kittest.Probe(http.MethodGet, in.Base+runtime.A2APath+runtime.CardPath, kittest.Token)
	var card a2a.AgentCard
	if status != http.StatusOK || json.Unmarshal([]byte(body), &card) != nil || card.Version != "9.9.9-test" || card.Name != "ax-adk" {
		t.Fatalf("card = %d %s", status, body)
	}
	if want := in.Base + "/a2a"; card.SupportedInterfaces[0].URL != want {
		t.Errorf("card URL = %s, want PUBLIC_URL's default, %s", card.SupportedInterfaces[0].URL, want)
	}
}

// Beside the AX task runner, /readyz and /metadata/ are the runner's.
func TestBesideTheRunner(t *testing.T) {
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/readyz":
			http.Error(w, "runner not ready", http.StatusServiceUnavailable)
		case strings.HasPrefix(r.URL.Path, "/metadata/"):
			_, _ = io.WriteString(w, "metadata "+r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(runner.Close)
	env := sqliteEnv(t)
	env["RUNNER_URL"] = runner.URL
	in := kittest.Start(t, kitSubject(), "ax-0", env, runtime.Options{})
	if got, body := kittest.Probe(http.MethodGet, in.Base+"/readyz", ""); got != http.StatusServiceUnavailable || !strings.Contains(body, "runner not ready") {
		t.Errorf("/readyz = %d %q, want the runner's 503", got, body)
	}
	if got, body := kittest.Probe(http.MethodGet, in.Base+"/metadata/task", ""); got != http.StatusOK || body != "metadata /metadata/task" {
		t.Errorf("/metadata/task = %d %q, want the runner's", got, body)
	}
	if got, _ := kittest.Probe(http.MethodGet, in.Base+"/healthz", ""); got != http.StatusOK {
		t.Errorf("/healthz = %d, want the agent's 200", got)
	}
}

// A token file the agent cannot read stops it before it listens.
func TestAnUnreadableTokenFileStopsTheAgent(t *testing.T) {
	s := kitSubject()
	env := map[string]string{"MODEL_REGION": "r", "MODEL_NAME": "m", "AUTH_TOKEN_FILE": filepath.Join(t.TempDir(), "absent"),
		"SQLITE_PATH": filepath.Join(t.TempDir(), "ax.db")}
	cfg, own, err := runtime.Parse(s.Addon, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	err = runtime.Run(t.Context(), s.Addon, cfg, own, runtime.Options{Log: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "AUTH_TOKEN_FILE") {
		t.Fatalf("Run = %v, want an AUTH_TOKEN_FILE error", err)
	}
}

// The agent registers its own metric in the telemetry the runtime serves.
func TestTheAgentRegistersItsOwnMetrics(t *testing.T) {
	in := kittest.Start(t, kitSubject(), "ax-0", sqliteEnv(t), runtime.Options{})
	kittest.WaitFor(t, "the agent to be ready", in.Ready)
	again := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_ax_sessions_deleted_total",
		Help: "ADK sessions the agent deleted, each with its context's tasks, by reason: idle (no activity for SESSION_IDLE_TTL)."},
		[]string{"reason"})
	var already prometheus.AlreadyRegisteredError
	if err := in.Tel.Register(again); !errors.As(err, &already) {
		t.Fatalf("registering agentkourier_ax_sessions_deleted_total again = %v, want AlreadyRegisteredError", err)
	}
}

// The runtime's idle sweep deletes a context's ADK session with its tasks, and the agent counts it.
func TestTheIdleSweepDeletesTheADKSessionAndCountsIt(t *testing.T) {
	env := sqliteEnv(t)
	env["SESSION_IDLE_TTL"] = "1h"
	in := kittest.Start(t, kitSubject(), "ax-0", env, runtime.Options{UpkeepInterval: 20 * time.Millisecond,
		Now: func() time.Time { return time.Now().Add(2 * time.Hour) }})
	kittest.WaitFor(t, "the agent to be ready", in.Ready)
	res, err := in.Client(t).SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))})
	task, ok := res.(*a2a.Task)
	if err != nil || !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("turn = %+v, %v", res, err)
	}
	kittest.WaitFor(t, "the idle session to be counted", func() bool {
		return in.Metric(`agentkourier_ax_sessions_deleted_total{reason="idle"}`) != ""
	})
	if err := in.Stop(t); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(t.Context(), env["SQLITE_PATH"], store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Sessions().Get(t.Context(), &session.GetRequest{AppName: store.AppName, UserID: "agent-kourier", SessionID: task.ContextID}); err == nil {
		t.Fatal("the idle context's ADK session is still there")
	}
}

// On SQLite, the agent fails the turns a restart cut off before it serves, and counts them: one whose stored record
// does not decode, which the store fails by its state alone, too.
func TestSQLiteSettlesAnUnreadableCutOffTurn(t *testing.T) {
	env := sqliteEnv(t)
	st, err := store.OpenSQLite(t.Context(), env["SQLITE_PATH"], store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []a2a.TaskID{"t1", "t2"} {
		if _, err := st.Create(kittest.AsCaller(), &a2a.Task{ID: id, ContextID: "c-" + string(id), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Write(t.Context(), func(q kitstore.Querier) error {
		_, err := q.Exec(t.Context(), `UPDATE a2a_tasks SET payload = ? WHERE id = ?`, []byte(`[1]`), "t2")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	in := kittest.Start(t, kitSubject(), "ax-0", env, runtime.Options{})
	kittest.WaitFor(t, "the agent to be ready", in.Ready)
	if got, err := in.Client(t).GetTask(t.Context(), &a2a.GetTaskRequest{ID: "t1"}); err != nil || got.Status.State != a2a.TaskStateFailed {
		t.Fatalf("the cut-off task = %+v, %v; want failed", got, err)
	}
	in.WantMetric(t, "agentkourier_ax_tasks_interrupted_total", "2")
	in.WantMetric(t, `agentkourier_ax_turns_total{outcome="interrupted"}`, "2")
}

// The add-on is named as package config's tests read its settings: ax, its process the agent, with the OIDC check.
func TestTheAddonsNames(t *testing.T) {
	a := Addon("v", nil)
	if a.Name != "ax" || a.Noun != "agent" || a.OIDC == nil || a.TelemetrySpec().Name != "ax" {
		t.Fatalf("Addon = %q, %q, OIDC %v", a.Name, a.Noun, a.OIDC != nil)
	}
}
