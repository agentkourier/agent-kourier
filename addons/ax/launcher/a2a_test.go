package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
)

type turnResponse struct {
	Result struct {
		Task *a2a.Task `json:"task"`
	} `json:"result"`
	Error *struct{ Code int } `json:"error"`
}

func TestA2AReplayAndBusyAdmission(t *testing.T) {
	e, _ := testLauncher(t)
	ctx, stop := context.WithTimeout(t.Context(), 12*time.Second)
	defer stop()
	e.cfg.timeout = 6 * time.Second
	first := make(chan struct{})
	release := make(chan struct{})
	var createCount atomic.Int32
	var mu sync.Mutex
	axTasks := make(map[string]*ax.Task)
	e.client = fakeAX{
		create: func(_ context.Context, task *ax.Task) (*ax.Task, error) {
			createCount.Add(1)
			if task.GetSpec().GetGoal().GetText() == "reject the create" {
				return nil, status.Error(codes.InvalidArgument, "fixture failure")
			}
			task = controlledTask(task)
			mu.Lock()
			axTasks[task.Metadata.Name] = task
			mu.Unlock()
			return task, nil
		},
		get: func(_ context.Context, name string) (*ax.Task, error) {
			mu.Lock()
			defer mu.Unlock()
			if task := axTasks[name]; task != nil {
				return task, nil
			}
			return nil, status.Error(codes.NotFound, "not found")
		},
		resume: func(ctx context.Context, name string) (*ax.Task, error) {
			mu.Lock()
			task := axTasks[name]
			mu.Unlock()
			if createCount.Load() == 1 {
				close(first)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			var id string
			for _, env := range task.Spec.Env {
				if env.Name == result.IDEnv {
					id = env.Value
				}
			}
			c := result.Claim{ResultID: id, TaskUID: task.Metadata.Uid, InvocationID: result.NewID()}
			if code, err := e.Claim(ctx, c); code != http.StatusNoContent || err != nil {
				t.Errorf("claim = %d, %v", code, err)
			}
			report := result.Report{Claim: c, Output: "fixture output", OutputSource: "file"}
			if code, err := e.Report(ctx, report, sha256.Sum256([]byte("fixture output"))); code != http.StatusNoContent || err != nil {
				t.Errorf("result = %d, %v", code, err)
			}
			return task, nil
		},
		delete: func(_ context.Context, name string) error {
			mu.Lock()
			defer mu.Unlock()
			delete(axTasks, name)
			return nil
		},
	}

	url := testEndpoint(ctx, t, e)
	send := func(id, prompt string) turnResponse {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(prompt))
		m.ID = id
		var reply turnResponse
		if err := testRPC(ctx, url, "SendMessage", &a2a.SendMessageRequest{Message: m}, &reply); err != nil {
			t.Error(err)
		}
		return reply
	}
	firstResult := make(chan turnResponse, 1)
	go func() { firstResult <- send("original", "hold the first turn") }()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first turn never started")
	}
	if reply := send("busy", "distinct turn"); reply.Error == nil || reply.Error.Code != -32004 {
		t.Fatalf("last slot did not return busy: %+v", reply.Error)
	}
	replayResult := make(chan turnResponse, 1)
	go func() { replayResult <- send("original", "hold the first turn") }()
	close(release)
	a, b := <-firstResult, <-replayResult
	if a.Result.Task == nil || b.Result.Task == nil || a.Result.Task.ID != b.Result.Task.ID || a.Result.Task.Status.State != a2a.TaskStateCompleted || createCount.Load() != 1 {
		t.Fatalf("replay returned different tasks or ran Create again: %+v (%+v) / %+v (%+v), creates=%d", a, a.Result.Task, b, b.Result.Task, createCount.Load())
	}

	waitForRelease(ctx, t, e)
	failed := send("failure", "reject the create")
	if failed.Result.Task == nil || failed.Result.Task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("invalid Create did not fail its turn: %+v", failed)
	}
	waitForRelease(ctx, t, e)
	after := send("after", "accepted after failure")
	if after.Result.Task == nil || after.Result.Task.Status.State != a2a.TaskStateCompleted || createCount.Load() != 3 {
		t.Fatalf("Create failure kept the slot: %+v, creates=%d", after, createCount.Load())
	}
}

func waitForRelease(ctx context.Context, t *testing.T, e *launcher) {
	t.Helper()
	for e.admitted.Load() != 0 {
		if !sleep(ctx, time.Millisecond) {
			t.Fatal("capacity reservation was not released")
		}
	}
}

func testEndpoint(ctx context.Context, t *testing.T, e *launcher) string {
	t.Helper()
	a := Addon("test")
	a.Settings = nil
	a.Open = func(context.Context, settings.Config, runtime.StoreOptions) (*store.Store, error) { return e.st, nil }
	a.Build = func(ctx context.Context, _ runtime.Env[*store.Store]) (runtime.Backend, error) {
		e.life, e.stop = context.WithCancel(ctx)
		return e, nil
	}
	cfg, own, err := runtime.Parse(a, func(k string) string {
		return map[string]string{"AUTH_DISABLED": "true", "LISTEN_ADDR": "127.0.0.1:0"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- runtime.Run(ctx, a, cfg, own, runtime.Options{Listening: func(addr net.Addr) { ready <- addr }})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	var addr net.Addr
	select {
	case addr = <-ready:
	case <-ctx.Done():
		t.Fatal("add-on did not listen")
	}
	base := "http://" + addr.String()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return base
			}
		}
		if !sleep(ctx, 5*time.Millisecond) {
			t.Fatal("add-on did not become ready")
		}
	}
}

func testRPC(ctx context.Context, base, method string, in, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": in})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/a2a", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("A2A HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out); err != nil {
		return errors.New("decode A2A response")
	}
	return nil
}
