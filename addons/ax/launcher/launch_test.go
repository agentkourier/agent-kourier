package launcher

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
)

type fakeAX struct {
	ax.AXClient
	create  func(context.Context, *ax.Task) (*ax.Task, error)
	get     func(context.Context, string) (*ax.Task, error)
	resume  func(context.Context, string) (*ax.Task, error)
	delete  func(context.Context, string) error
	suspend func(context.Context, string) (*ax.Task, error)
}

func (f fakeAX) CreateTask(ctx context.Context, r *ax.CreateTaskRequest, _ ...grpc.CallOption) (*ax.Task, error) {
	return f.create(ctx, r.Task)
}
func (f fakeAX) GetTask(ctx context.Context, r *ax.GetTaskRequest, _ ...grpc.CallOption) (*ax.Task, error) {
	return f.get(ctx, r.Name)
}
func (f fakeAX) ResumeTask(ctx context.Context, r *ax.ResumeTaskRequest, _ ...grpc.CallOption) (*ax.Task, error) {
	return f.resume(ctx, r.Name)
}
func (f fakeAX) DeleteTask(ctx context.Context, r *ax.DeleteTaskRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, f.delete(ctx, r.Name)
}
func (f fakeAX) SuspendTask(ctx context.Context, r *ax.SuspendTaskRequest, _ ...grpc.CallOption) (*ax.Task, error) {
	return f.suspend(ctx, r.Name)
}

func testLauncher(t *testing.T) (*launcher, context.Context) {
	t.Helper()
	s, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "ax.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	e := &launcher{
		cfg: config{atespace: "fixture", identity: "launcher", url: "https://callback.test" + result.Path, promptKey: "prompt",
			history: true, running: 1, poll: time.Millisecond, timeout: time.Minute, keep: time.Minute},
		template: &ax.Task{ApiVersion: "ax.io/v1alpha1", Kind: "Task", Spec: &ax.TaskSpec{
			Profile: &ax.Profile{Image: "registry.test/agent@sha256:" + strings.Repeat("a", 64)}, Gateway: &ax.GatewayRef{Name: "gateway"},
			Commands: []string{"/usr/local/bin/agent-kourier-ax-result", "--", "/usr/local/bin/agent"},
		}},
		st: s, log: slog.New(slog.DiscardHandler), now: time.Now, life: t.Context(),
		rpc:     prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_rpc"}, []string{"method", "code"}),
		trimmed: prometheus.NewCounter(prometheus.CounterOpts{Name: "test_trimmed"}),
		cleanup: prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_cleanup"}),
	}
	ctx, call := a2asrv.NewCallContext(t.Context(), nil)
	call.User = a2asrv.NewAuthenticatedUser("owner", nil)
	return e, ctx
}

func seedLaunch(ctx context.Context, t *testing.T, e *launcher, id string) (store.Launch, *ax.Task) {
	t.Helper()
	info := &a2asrv.ExecutorContext{TaskID: a2a.TaskID(id), ContextID: "thread"}
	task := a2a.NewSubmittedTask(info, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("prompt")))
	if _, err := e.st.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	rid, deadline := result.NewID(), e.now().Add(time.Minute)
	filled, err := fillTask(e.cfg, e.template, shortHash(id), shortHash("thread"), rid, deadline, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	l := store.Launch{TaskID: id, Name: filled.Metadata.Name, ResultID: rid, Deadline: deadline.UnixNano(), Fingerprint: fingerprint(filled.Spec)}
	if ok, err := e.st.Register(ctx, l); !ok || err != nil {
		t.Fatalf("register = %t, %v", ok, err)
	}
	return l, filled
}

func controlledTask(task *ax.Task) *ax.Task {
	task = proto.CloneOf(task)
	task.Metadata.Uid = "ax-task-uid"
	task.Status = &ax.TaskStatus{ActorUid: "ax-actor-uid", State: ax.TaskState_TASK_STATE_SUSPENDED}
	return task
}

func TestReconcileAmbiguousAXCalls(t *testing.T) {
	for _, test := range []struct {
		name         string
		claimOnFirst bool
		replace      string
		wantResume   int
	}{
		{"claim before lost response", true, "", 1},
		{"running actor without delivery", false, "", 2},
		{"actor lookup unavailable", false, "unavailable", 2},
		{"different actor", false, "actor", 1},
		{"occupied runner", false, "occupied", 1},
		{"unrelated task after create", false, "task", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, ctx := testLauncher(t)
			l, template := seedLaunch(ctx, t, e, "a2a")
			var existing *ax.Task
			createN, getN, resumeN := 0, 0, 0
			e.client = fakeAX{
				create: func(_ context.Context, task *ax.Task) (*ax.Task, error) {
					createN++
					existing = controlledTask(task)
					if test.replace == "task" {
						existing.Metadata.Labels[nameLabel] = "another"
					}
					return nil, status.Error(codes.Unavailable, "lost accepted Create")
				},
				get: func(context.Context, string) (*ax.Task, error) {
					getN++
					if test.replace == "unavailable" && getN > 1 && getN <= maxResumeAttempts+1 {
						return nil, status.Error(codes.Unavailable, "actor lookup unavailable")
					}
					return proto.CloneOf(existing), nil
				},
				resume: func(ctx context.Context, _ string) (*ax.Task, error) {
					resumeN++
					existing.Status.State = ax.TaskState_TASK_STATE_RUNNING
					if test.replace == "occupied" {
						return nil, status.Error(codes.Internal, "runner is running another task")
					}
					if test.replace == "actor" {
						existing.Status.ActorUid = "new-actor"
					}
					if test.claimOnFirst {
						c := result.Claim{ResultID: l.ResultID, TaskUID: existing.Metadata.Uid, InvocationID: result.NewID()}
						if got, err := e.st.Claim(ctx, c); got != 204 || err != nil {
							t.Errorf("start before Resume reply: %d, %v", got, err)
						}
						report := result.Report{Claim: c, Output: "fast result", OutputSource: "file"}
						if got, _, err := e.st.AcceptResult(ctx, report, sha256.Sum256([]byte("result"))); got != 204 || err != nil {
							t.Errorf("result before Resume reply: %d, %v", got, err)
						}
					}
					if resumeN == 1 {
						return nil, status.Error(codes.Unavailable, "lost Resume reply")
					}
					return existing, nil
				},
			}
			e.launch(ctx, l, template)
			if createN != 1 || resumeN != test.wantResume {
				t.Fatalf("Create=%d Resume=%d, want 1 and %d", createN, resumeN, test.wantResume)
			}
			got, err := e.st.Launch(ctx, l.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if test.claimOnFirst && got.Decision != store.Succeeded {
				t.Fatalf("fast receipt lost: %s", got.Decision)
			}
			if test.wantResume < 2 && !test.claimOnFirst && got.Decision != store.Failed {
				t.Fatalf("replacement outcome = %s", got.Decision)
			}
		})
	}
}

func TestCleanupReconcilesLateCreateAndRetriesDelete(t *testing.T) {
	e, ctx := testLauncher(t)
	l, template := seedLaunch(ctx, t, e, "late")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var existing *ax.Task
	deletes := 0
	e.client = fakeAX{
		create: func(_ context.Context, task *ax.Task) (*ax.Task, error) {
			close(entered)
			<-release
			existing = controlledTask(task)
			return existing, nil
		},
		get: func(context.Context, string) (*ax.Task, error) {
			if existing == nil {
				return nil, status.Error(codes.NotFound, "not visible")
			}
			return existing, nil
		},
		delete: func(context.Context, string) error {
			deletes++
			if deletes == 1 {
				return status.Error(codes.Unavailable, "retry")
			}
			existing = nil
			return nil
		},
	}
	e.active.Store(l.TaskID, struct{}{})
	go func() {
		defer close(done)
		defer e.active.Delete(l.TaskID)
		e.launch(ctx, l, template)
	}()
	<-entered
	if ok, err := e.st.RequestCancel(ctx, l.TaskID); !ok || err != nil {
		t.Fatalf("cancel %t, %v", ok, err)
	}
	if err := e.st.ScheduleCleanup(ctx, l.TaskID, e.now()); err != nil {
		t.Fatal(err)
	}
	e.cleanPending(ctx)
	provisional, err := e.st.Launch(ctx, l.TaskID)
	if err != nil || provisional.Clean != 0 {
		t.Fatal("cleanup was cleared before Create returned")
	}
	close(release)
	<-done
	for range 3 {
		e.cleanPending(ctx)
	}
	settled, err := e.st.Launch(ctx, l.TaskID)
	if err != nil || settled.Clean != 1 || deletes != 2 || existing != nil {
		t.Fatalf("cleanup result clean=%d, deletes=%d, err=%v", settled.Clean, deletes, err)
	}
}

func TestLastSlotReservationIsAtomic(t *testing.T) {
	e, ctx := testLauncher(t)
	releases := make(chan func(), 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			release, _ := e.AdmitMessage(ctx, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("prompt")))
			releases <- release
		})
	}
	wg.Wait()
	close(releases)
	var accepted int
	for release := range releases {
		if release != nil {
			accepted++
			release()
		}
	}
	if accepted != 1 {
		t.Fatalf("%d messages reserved the last slot", accepted)
	}
	if release, err := e.AdmitMessage(ctx, &a2a.Message{}); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

func TestObservedProgressIsAnnouncedOnce(t *testing.T) {
	e, ctx := testLauncher(t)
	l, template := seedLaunch(ctx, t, e, "progress")
	runner := controlledTask(template)
	if ok, err := e.st.Bind(ctx, l.TaskID, runner.Metadata.Uid, runner.Status.ActorUid); !ok || err != nil {
		t.Fatalf("bind: %t, %v", ok, err)
	}
	if ok, err := e.st.Dispatch(ctx, l.TaskID, true, e.now().Add(time.Minute)); !ok || err != nil {
		t.Fatalf("dispatch: %t, %v", ok, err)
	}
	claim := result.Claim{ResultID: l.ResultID, TaskUID: runner.Metadata.Uid, InvocationID: result.NewID()}
	if got, err := e.st.Claim(ctx, claim); got != 204 || err != nil {
		t.Fatalf("claim: %d, %v", got, err)
	}
	states := []ax.TaskState{
		ax.TaskState_TASK_STATE_RESUMING, ax.TaskState_TASK_STATE_QUEUED, ax.TaskState_TASK_STATE_QUEUED,
		ax.TaskState_TASK_STATE_RUNNING, ax.TaskState_TASK_STATE_RUNNING,
	}
	next := 0
	e.client = fakeAX{get: func(ctx context.Context, _ string) (*ax.Task, error) {
		if next == len(states) {
			r := result.Report{Claim: claim, Output: "done", OutputSource: "file"}
			if status, _, err := e.st.AcceptResult(ctx, r, sha256.Sum256([]byte("done"))); status != 204 || err != nil {
				t.Fatalf("result %d, %v", status, err)
			}
		} else {
			runner.Status.State = states[next]
			next++
		}
		return runner, nil
	}}
	var notices []string
	var terminal *a2a.Task
	started, err := e.st.Get(ctx, a2a.TaskID(l.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	e.follow(ctx, started.Task, l, e.now().Add(time.Second), nil, func(ev a2a.Event, err error) bool {
		if err != nil {
			t.Fatal(err)
		}
		switch ev := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			text := ev.Status.Message.Parts[0].Text()
			if text == "AX sandbox queued." && runner.Status.State != ax.TaskState_TASK_STATE_QUEUED {
				t.Fatalf("announced queued while AX reported %s", runner.Status.State)
			}
			notices = append(notices, text)
		case *a2a.Task:
			terminal = ev
		}
		return true
	})
	if strings.Join(notices, "|") != "AX sandbox queued.|AX sandbox running." || terminal == nil || terminal.Status.State != a2a.TaskStateCompleted ||
		len(terminal.Artifacts) != 1 || terminal.Artifacts[0].Parts[0].Text() != "done" {
		t.Fatalf("progress %q, terminal %v", notices, terminal)
	}
}

func TestHistoryKeepsNewestAndScopesToCallerAndThread(t *testing.T) {
	e, ctx := testLauncher(t)
	seed := func(ctx context.Context, id, thread, input, answer string) {
		t.Helper()
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(input))
		task := &a2a.Task{ID: a2a.TaskID(id), ContextID: thread, History: []*a2a.Message{msg},
			Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, Artifacts: []*a2a.Artifact{{ID: "output", Parts: []*a2a.Part{a2a.NewTextPart(answer)}}}}
		if _, err := e.st.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	seed(ctx, "old", "thread", "old-marker"+strings.Repeat("a", maxConfigSize), "old-answer")
	seed(ctx, "new", "thread", "new-marker", "new-answer")
	seed(ctx, "different", "other-thread", "other-thread-marker", "")
	other, call := a2asrv.NewCallContext(t.Context(), nil)
	call.User = a2asrv.NewAuthenticatedUser("another", nil)
	seed(other, "different-user", "thread", "other-user-marker", "")
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("current-message"))
	exec := &a2asrv.ExecutorContext{TaskID: "current", ContextID: "thread", User: a2asrv.NewAuthenticatedUser("owner", nil), Message: msg}
	task, _, err := e.taskForTurn(ctx, exec, result.NewID(), e.now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	goal := task.Spec.Goal.Text
	for _, want := range []string{"new-marker", "new-answer", "current-message"} {
		if !strings.Contains(goal, want) {
			t.Fatalf("goal is missing %s", want)
		}
	}
	for _, unwanted := range []string{"old-marker", "other-thread-marker", "other-user-marker"} {
		if strings.Contains(goal, unwanted) {
			t.Fatalf("%s leaked into the goal", unwanted)
		}
	}
	e.cfg.history = false
	filled, _, err := e.taskForTurn(ctx, exec, result.NewID(), e.now().Add(time.Minute))
	if err != nil || filled.Spec.Goal.Text != "current-message" || filled.Spec.SessionId != task.Spec.SessionId {
		t.Fatalf("history opt-out changed the session or prompt: %v", err)
	}
	large := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(strings.Repeat("a", maxConfigSize)))
	if err := e.ValidateMessage(ctx, large, nil); err == nil {
		t.Fatal("oversized current prompt passed admission")
	}
}

func TestCancelDuringResumeClosesClaimAndWaitsForRPC(t *testing.T) {
	e, ctx := testLauncher(t)
	l, template := seedLaunch(ctx, t, e, "late-resume")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var existing *ax.Task
	deletes := 0
	e.client = fakeAX{
		create: func(_ context.Context, task *ax.Task) (*ax.Task, error) {
			existing = controlledTask(task)
			return existing, nil
		},
		get: func(context.Context, string) (*ax.Task, error) {
			if existing == nil {
				return nil, status.Error(codes.NotFound, "deleted")
			}
			return existing, nil
		},
		resume: func(context.Context, string) (*ax.Task, error) {
			close(entered)
			<-release
			return existing, nil
		},
		delete: func(context.Context, string) error {
			deletes++
			existing = nil
			return nil
		},
	}
	e.active.Store(l.TaskID, struct{}{})
	go func() {
		defer close(done)
		defer e.active.Delete(l.TaskID)
		e.launch(ctx, l, template)
	}()
	<-entered
	if ok, err := e.st.RequestCancel(ctx, l.TaskID); !ok || err != nil {
		t.Fatalf("cancel %t, %v", ok, err)
	}
	claim := result.Claim{ResultID: l.ResultID, TaskUID: existing.Metadata.Uid, InvocationID: result.NewID()}
	if got, err := e.st.Claim(ctx, claim); got != 410 || err != nil {
		t.Fatalf("late start = %d, %v", got, err)
	}
	if err := e.st.ScheduleCleanup(ctx, l.TaskID, e.now()); err != nil {
		t.Fatal(err)
	}
	e.cleanPending(ctx)
	if deletes != 0 {
		t.Fatal("cleanup overlapped a Resume that can clear deletion")
	}
	close(release)
	<-done
	e.cleanPending(ctx)
	e.cleanPending(ctx)
	settled, err := e.st.Launch(ctx, l.TaskID)
	if err != nil || settled.Decision != store.Canceled || settled.Clean != 1 || deletes != 1 {
		t.Fatalf("late Resume cleanup decision=%q clean=%d deletes=%d err=%v", settled.Decision, settled.Clean, deletes, err)
	}
}
