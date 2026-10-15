package interact

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// blockingAudit pauses the first repair append, allowing concurrent recovery callers to overlap their audit lookup before
// the append is released. Without the intake's audit mutex, callers that observe the missing row can append duplicates; the
// final persisted-row assertion guards this repair boundary.
type blockingAudit struct {
	store.Audit
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingAudit) AppendAudit(ctx context.Context, e store.AuditEntry) error {
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.Audit.AppendAudit(ctx, e)
}

func TestTimeoutAuditRepairIsSerialized(t *testing.T) {
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blocked := &blockingAudit{Audit: st, started: make(chan struct{}), release: make(chan struct{})}
	w, err := audit.New(blocked, audit.Options{OnFailure: audit.NoFailure, Now: func() time.Time { return created.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	i := &Intake{cfg: Config{Audit: w}, auditReader: blocked}
	row := store.Interaction{ID: "ix-repair", TaskID: "task-1", CreatedAt: created}
	const callers = 8
	ready := make(chan struct{}, callers)
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			errs <- i.ensureTimeoutAudit(t.Context(), "ns/b", "agent-service", row)
		}()
	}
	for range callers {
		<-ready
	}
	close(start)
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		close(blocked.release)
		wg.Wait()
		t.Fatal("timeout audit repair did not start")
	}
	close(blocked.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("timeout audit repair: %v", err)
		}
	}
	entries, err := st.ListAudit(t.Context(), time.Time{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Subject != "interaction=ix-repair task=task-1" || entries[0].Outcome != "timed_out" {
		t.Fatalf("audit entries %+v; want one persisted timeout decision", entries)
	}
}
