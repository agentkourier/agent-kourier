package launcher

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/kittest"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

func TestLauncherKitConformance(t *testing.T) {
	subject := launcherSubject(t)
	t.Run("SQLite", func(t *testing.T) { kittest.RunSQLite(t, subject) })
	t.Run("Postgres", func(t *testing.T) { kittest.RunPostgres(t, subject) })
}

func launcherEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	template, token := filepath.Join(dir, "task.yaml"), filepath.Join(dir, "result-token")
	body := `apiVersion: ax.io/v1alpha1
kind: Task
spec:
  gateway: {name: results}
  commands: [agent-kourier-ax-result, --, agent-command]
  profile: {image: registry.test/agent@sha256:` + strings.Repeat("a", 64) + `}
`
	for path, content := range map[string]string{template: body, token: "fake-ax-callback-token"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{
		"AX_ADDR": "127.0.0.1:1", "AX_TASK_TEMPLATE": template, "AX_RESULT_TOKEN_FILE": token,
		"AX_RESULT_URL": "https://callback.test/ax/results/v1", "AX_LAUNCHER_ID": "test", "AX_POLL_INTERVAL": "10ms",
	}
}

func launcherSubject(t *testing.T) kittest.Subject[*store.Store] {
	t.Helper()
	addon := Addon("test")
	var tasks sync.Map
	addon.Build = func(ctx context.Context, env runtime.Env[*store.Store]) (runtime.Backend, error) {
		backend, err := build(ctx, env)
		if err != nil {
			return nil, err
		}
		e := backend.(*launcher)
		get := func(_ context.Context, name string) (*ax.Task, error) {
			if task, ok := tasks.Load(name); ok {
				return task.(*ax.Task), nil
			}
			return nil, status.Error(codes.NotFound, "fake task absent")
		}
		e.client = fakeAX{
			create: func(_ context.Context, task *ax.Task) (*ax.Task, error) {
				created := controlledTask(task)
				created.Status.State = ax.TaskState_TASK_STATE_RUNNING
				tasks.Store(created.GetMetadata().GetName(), created)
				return created, nil
			},
			get: get,
			resume: func(ctx context.Context, name string) (*ax.Task, error) {
				task, err := get(ctx, name)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(task.GetSpec().GetGoal().GetText(), "hold this turn") {
					return task, nil
				}
				claim := result.Claim{TaskUID: task.GetMetadata().GetUid(), InvocationID: result.NewID()}
				for _, variable := range task.GetSpec().GetEnv() {
					if variable.GetName() == result.IDEnv {
						claim.ResultID = variable.GetValue()
					}
				}
				if code, err := e.Claim(ctx, claim); code != http.StatusNoContent || err != nil {
					return nil, status.Errorf(codes.FailedPrecondition, "fake start claim returned %d", code)
				}
				const output = "fixture"
				report := result.Report{Claim: claim, Output: output, OutputSource: "file"}
				if code, err := e.Report(ctx, report, sha256.Sum256([]byte(output))); code != http.StatusNoContent || err != nil {
					return nil, status.Errorf(codes.FailedPrecondition, "fake report returned %d", code)
				}
				return task, nil
			},
			delete: func(_ context.Context, name string) error {
				tasks.Delete(name)
				return nil
			},
		}
		return e, nil
	}
	return kittest.Subject[*store.Store]{Addon: addon, Env: launcherEnv(t),
		Complete: "complete this turn", Hold: "hold this turn", BadMessage: []*a2a.Part{a2a.NewDataPart(map[string]any{"bad": true})}}
}

func TestBuildRejectsTheA2ATokenAsCallbackCredential(t *testing.T) {
	env := launcherEnv(t)
	env["AUTH_TOKEN_FILE"] = env["AX_RESULT_TOKEN_FILE"]
	cfg, own, err := runtime.Parse(Addon("test"), func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	_, err = build(t.Context(), runtime.Env[*store.Store]{Config: cfg, Settings: own, Log: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("build error = %v, want distinct credentials", err)
	}
}
