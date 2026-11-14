package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// staleSource is a per-object source that can say its view lags, as an informer cache does while the API server is out.
type staleSource struct {
	perObjectSource
	err atomic.Pointer[error]
}

func (s *staleSource) Stale() error {
	if p := s.err.Load(); p != nil {
		return *p
	}
	return nil
}

// A source that says its view lags makes the Reloader stale for as long as it says so, and the reloads' own record is not
// touched: the stale flag clears when the source clears it, with no reload. A Source that is not a StaleSource, a directory,
// is never stale for this.
func TestStatusIsStaleWhileTheSourceSaysItLags(t *testing.T) {
	src := &staleSource{perObjectSource: perObjectSource{t: t, make: generations, fp: "v1"}}
	r := newPerObjectReloader(src, specSecrets(), nil)
	if err := r.reload(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.Stale || st.LastSuccess.IsZero() {
		t.Fatalf("Status after a good load = %+v; want fresh", st)
	}
	lag := errors.New("bindings.agentkourier.dev: forbidden")
	src.err.Store(&lag)
	if st := r.Status(); !st.Stale || st.Errors != 0 || st.LastSuccess.IsZero() {
		t.Errorf("Status while the source lags = %+v; want stale with no reload error", st)
	}
	if err := r.reload(t.Context(), true); err != nil { // a recheck while it lags must not record the lag as its own
		t.Fatal(err)
	}
	src.err.Store(nil)
	if st := r.Status(); st.Stale {
		t.Errorf("Status after the source recovered = %+v; want fresh again, with no reload", st)
	}

	files := newTestReloader(specDir(t), specSecrets())
	mustReload(t, files, true)
	if files.Status().Stale {
		t.Error("a Reloader of files is stale")
	}
}

// editedWhileRead is a source that is edited without pause: each read hands out the config of one generation of sre-alerts,
// under the fingerprint of that generation, and an edit lands before the read is over, so that the fingerprint has moved on by
// the time the Reloader looks at it again.
type editedWhileRead struct {
	t    *testing.T
	make func(*testing.T) *Set
	gen  int64
	sets int
}

func (e *editedWhileRead) fp() string { return fmt.Sprintf("generation-%d", e.gen) }

func (e *editedWhileRead) Set(context.Context) (*Set, string, error) {
	e.sets++
	set := e.make(e.t)
	sreAlerts(set).Generation = e.gen
	read := e.fp()
	e.gen++ // the edit
	return set, read, nil
}

func (e *editedWhileRead) Fingerprint(context.Context) string { return e.fp() }

// servedGeneration is the generation of sre-alerts that r serves.
func servedGeneration(t *testing.T, r *Reloader) int64 {
	t.Helper()
	snap := r.Current()
	if snap == nil {
		t.Fatal("the Reloader serves no snapshot")
	}
	rb, ok := snap.Binding("sre", "sre-alerts")
	if !ok {
		t.Fatal("the snapshot has no sre-alerts")
	}
	return rb.Binding.Generation
}

// A custom resource can change at any moment, so a source that is edited without pause must not stop a per-object Reloader from
// publishing, at Start or later: it serves each read as it was read, and the next poll finds what was edited since. The
// Reloader of files, which reads a directory that can change under the read, still reads again, gives up when it moves again,
// and publishes nothing, as before.
func TestAPerObjectReloaderPublishesWhatItReadFromASourceThatIsEditedWithoutPause(t *testing.T) {
	src := &editedWhileRead{t: t, make: generations, gen: 10}
	r := newPerObjectReloader(src, specSecrets(), func(c *ReloaderConfig) { c.PollInterval = time.Hour })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start of a per-object Reloader whose source is edited during every read = %v; want it to publish that read", err)
	}
	cancel()
	r.Wait()
	if got := servedGeneration(t, r); got != 10 || src.sets != 1 {
		t.Errorf("after Start: serving generation %d after %d reads; want the one read, generation 10", got, src.sets)
	}
	if r.lastFingerprint != "generation-10" {
		t.Errorf("fingerprint kept after Start = %q; want that of the read, so that the next poll finds the edit", r.lastFingerprint)
	}
	for want := int64(11); want <= 13; want++ {
		mustReload(t, r, false) // a poll
		if got, st := servedGeneration(t, r), r.Status(); got != want || st.Stale || st.Errors != 0 {
			t.Errorf("poll: serving generation %d, status %+v; want generation %d, fresh, with no error", got, st, want)
		}
	}

	files := NewReloader(ReloaderConfig{Source: &editedWhileRead{t: t, make: generations, gen: 10}, Secrets: specSecrets(), Logger: testutil.DiscardLogger()})
	if err := files.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "changed during the load") || files.Current() != nil {
		t.Errorf("Start of a Reloader that is not per-object, over a source edited during every read = %v, serving a snapshot %v; want it to fail, serving none, as before", err, files.Current() != nil)
	}
}
