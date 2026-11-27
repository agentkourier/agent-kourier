package launcher

import (
	"context"
	"testing"
	"time"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
)

func TestSweepDeletesOnlyOldScopedOrphans(t *testing.T) {
	e, ctx := testLauncher(t)
	now := e.now()
	cutoff := now.Add(-maxDeadline - unknownRPCTime)
	makeTask := func(id string, created time.Time) *ax.Task {
		t.Helper()
		task, err := fillTask(e.cfg, e.template, shortHash(id), shortHash("test"), result.NewID(), now.Add(time.Minute), "test")
		if err != nil {
			t.Fatal(err)
		}
		task = controlledTask(task)
		task.Metadata.CreateTime = timestamppb.New(created)
		return task
	}
	old := makeTask("old", cutoff.Add(-time.Second))
	recent := makeTask("recent", cutoff.Add(time.Second))
	replaced := makeTask("replaced", cutoff.Add(-time.Second))
	wrongUID := makeTask("replaced", cutoff.Add(-time.Second))
	wrongUID.Metadata.Uid = "different-uid"
	var deleted []string
	e.client = fakeAX{
		list: func(context.Context, *ax.ListTasksRequest) (*ax.ListTasksResponse, error) {
			return &ax.ListTasksResponse{Tasks: []*ax.Task{old, recent, replaced}}, nil
		},
		get: func(_ context.Context, name string) (*ax.Task, error) {
			if name == old.GetMetadata().GetName() {
				return old, nil
			}
			return wrongUID, nil
		},
		delete: func(_ context.Context, name string) error {
			deleted = append(deleted, name)
			return nil
		},
	}
	e.sweep(ctx)
	if len(deleted) != 1 || deleted[0] != old.GetMetadata().GetName() {
		t.Fatalf("deleted %v, want the old orphan alone", deleted)
	}
}

func TestCleanupSuspendsRetainedTasksAndProtectsOwnership(t *testing.T) {
	e, ctx := testLauncher(t)
	intent, task := seedLaunch(ctx, t, e, "retained")
	task = controlledTask(task)
	if ok, err := e.st.Bind(ctx, intent.TaskID, task.GetMetadata().GetUid(), task.GetStatus().GetActorUid()); !ok || err != nil {
		t.Fatalf("bind = %v, %v", ok, err)
	}
	if err := e.st.ScheduleCleanup(ctx, intent.TaskID, e.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	suspended := 0
	deleted := 0
	e.client = fakeAX{
		get: func(context.Context, string) (*ax.Task, error) { return task, nil },
		suspend: func(context.Context, string) (*ax.Task, error) {
			suspended++
			return task, nil
		},
		delete: func(context.Context, string) error { deleted++; return nil },
	}
	e.cleanPending(ctx)
	retained, err := e.st.Launch(ctx, intent.TaskID)
	if err != nil || retained.Suspended == 0 || suspended != 1 {
		t.Fatalf("suspensions = %d, record = %v", suspended, err)
	}

	e.cleanPending(ctx)
	metrics := prometheus.NewRegistry()
	metrics.MustRegister(e.cleanup, e.retained)
	families, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	wantGauges := map[string]float64{
		"agentkourier_ax_cleanup_pending":  0,
		"agentkourier_ax_cleanup_retained": 1,
	}
	for _, family := range families {
		name := family.GetName()
		got := family.GetMetric()[0].GetGauge().GetValue()
		if want, ok := wantGauges[name]; !ok || got != want {
			t.Fatalf("%s = %g, want %g", name, got, want)
		}
		delete(wantGauges, name)
	}
	if len(wantGauges) != 0 {
		t.Fatalf("missing gauges %v", wantGauges)
	}
	if err := e.st.MarkClean(ctx, intent.TaskID); err != nil {
		t.Fatal(err)
	}
	// Once the retention window expires, a late reappearance gets deleted.
	e.now = func() time.Time { return time.Unix(0, retained.CleanupAt) }
	e.cleanLateTask(ctx, task, e.now())
	if deleted != 1 {
		t.Fatalf("deletions = %d, want one", deleted)
	}
	task.Metadata.Uid = "unrelated-task"
	e.cleanLaunch(ctx, retained, retained.CleanupAt)
	got, err := e.st.Launch(ctx, intent.TaskID)
	if err != nil || got.Clean != store.CleanForeign || deleted != 1 {
		t.Fatalf("state = %v, deletions = %d, error = %v", got.Clean, deleted, err)
	}
}
