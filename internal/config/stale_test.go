package config

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// movingSource is a source that changes under the first loads: each Fingerprint of the first moves says something new.
type movingSource struct {
	perObjectSource
	moves int
}

func (m *movingSource) Fingerprint(context.Context) string {
	if m.moves > 0 {
		m.moves--
		m.fp += "x"
	}
	return m.fp
}

// A custom resource can change at any moment, so a first load that finds the config moving goes on trying, for a while, where
// the files' Reloader fails at once: a start that exits for it would restart into the same chance.
func TestStartOfAPerObjectReloaderRetriesAMovingFirstLoad(t *testing.T) {
	mk := func() *movingSource {
		return &movingSource{perObjectSource: perObjectSource{t: t, make: generations, fp: "v"}, moves: 3}
	}
	r := newPerObjectReloader(mk(), specSecrets(), func(c *ReloaderConfig) { c.FirstLoadWait = 5 * time.Second })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start of a per-object Reloader = %v; want it to try again until the config holds still", err)
	}
	cancel()
	r.Wait()

	files := NewReloader(ReloaderConfig{Source: mk(), Secrets: specSecrets(), Logger: slog.New(slog.DiscardHandler)})
	if err := files.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "changed during the load") {
		t.Errorf("Start of a files Reloader = %v; want it to fail with a config that moves, as before", err)
	}

	short := newPerObjectReloader(&movingSource{perObjectSource: perObjectSource{t: t, make: generations, fp: "v"}, moves: 1000}, specSecrets(), func(c *ReloaderConfig) { c.FirstLoadWait = 300 * time.Millisecond })
	if err := short.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "changed during the load") {
		t.Errorf("Start of a per-object Reloader whose config never holds still = %v; want it to give up after FirstLoadWait", err)
	}
}
