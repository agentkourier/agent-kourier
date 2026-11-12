package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// fakeSource is a Source that is not a directory: it hands out a fresh Set of spec 7's examples
// (Resolve consumes the Set it is given) under whatever fingerprint the test sets.
type fakeSource struct {
	t      *testing.T
	fp     string      // what Fingerprint returns, and what Set reports as read
	setErr error       // what Set returns, with fp, instead of a Set
	sets   int         // calls of Set
	onSet  func(n int) // runs inside the nth Set, to change the source while it is read
}

func (f *fakeSource) Set(ctx context.Context) (*Set, string, error) {
	f.sets++
	read := f.fp // the state this read saw, before onSet moves it
	if f.onSet != nil {
		f.onSet(f.sets)
	}
	if f.setErr != nil {
		return nil, read, f.setErr
	}
	set, err := Load(ctx, specDir(f.t))
	if err != nil {
		f.t.Fatal(err)
	}
	return set, read, nil
}

func (f *fakeSource) Fingerprint(context.Context) string { return f.fp }

func newFakeReloader(src Source, secrets SecretResolver) *Reloader {
	// Dir names nothing: a Source overrides it, and a Reloader that read it would fail.
	return NewReloader(ReloaderConfig{Dir: "/nonexistent/config", Source: src, Secrets: secrets, Logger: testutil.DiscardLogger()})
}

// A Reloader needs no directory: it rebuilds when the source's fingerprint moves, not when it
// holds, and a forced recheck rebuilds either way.
func TestReloaderRebuildsOnFingerprintChange(t *testing.T) {
	src := &fakeSource{t: t, fp: "v1"}
	r := newFakeReloader(src, specSecrets())
	ctx, cancel := context.WithCancel(t.Context())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel() // the first load is all this test needs from Start
	r.Wait()
	first, rev := r.Current(), r.Revision(t.Context())
	if first == nil || src.sets != 1 {
		t.Fatalf("after Start: snapshot %v, %d reads of the source; want one", first, src.sets)
	}

	mustReload(t, r, false)
	if r.Current() != first || src.sets != 1 || r.Revision(t.Context()) != rev {
		t.Errorf("unchanged fingerprint: %d reads, snapshot replaced %v", src.sets, r.Current() != first)
	}

	src.fp = "v2"
	mustReload(t, r, false)
	if r.Current() == first || src.sets != 2 || r.Revision(t.Context()) == rev {
		t.Errorf("changed fingerprint: %d reads, snapshot replaced %v, revision moved %v", src.sets, r.Current() != first, r.Revision(t.Context()) != rev)
	}

	second := r.Current()
	mustReload(t, r, true)
	if r.Current() == second || src.sets != 3 {
		t.Errorf("forced recheck: %d reads, snapshot replaced %v; want a rebuild", src.sets, r.Current() != second)
	}
}

// A failed read or resolve keeps the last good snapshot and marks the Reloader stale. The same
// failing state is not tried again, and a change to the source is.
func TestReloaderKeepsLastGoodOnSourceFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*fakeSource, fakeSecrets)
	}{
		{"source cannot be read", func(f *fakeSource, _ fakeSecrets) { f.setErr = errors.New("read failed") }},
		{"objects do not resolve", func(_ *fakeSource, s fakeSecrets) { delete(s, "sre/agent-kourier-sre-alerts-oidc") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secrets := specSecrets()
			src := &fakeSource{t: t, fp: "v1"}
			var seen []error
			r := NewReloader(ReloaderConfig{Source: src, Secrets: secrets, Logger: testutil.DiscardLogger(), OnError: func(err error) { seen = append(seen, err) }})
			mustReload(t, r, true)
			good, st := r.Current(), r.Status()

			src.fp = "v2"
			tc.damage(src, secrets)
			for range 3 {
				_ = r.reload(t.Context(), false)
			}
			bad := r.Status()
			if r.Current() != good || !bad.Stale || bad.Errors != 1 || len(seen) != 1 || !bad.LastSuccess.Equal(st.LastSuccess) || src.sets != 2 {
				t.Errorf("after a failure: snapshot kept %v, status %+v, %d errors seen, %d reads; want last good, stale, one error, two reads", r.Current() == good, bad, len(seen), src.sets)
			}

			src.fp, src.setErr = "v3", nil
			secrets["sre/agent-kourier-sre-alerts-oidc"] = []string{"token"}
			mustReload(t, r, false)
			if fixed := r.Status(); r.Current() == good || fixed.Stale || fixed.Errors != 1 {
				t.Errorf("after the fix: snapshot replaced %v, status %+v", r.Current() != good, fixed)
			}
		})
	}
}

// A source that changes while the objects are resolved is read again, once. One that changes
// again is not trusted and is not a config error: the next poll tries it.
func TestReloaderReadsAMovingSourceAgain(t *testing.T) {
	src := &fakeSource{t: t, fp: "v1"}
	src.onSet = func(n int) {
		if n == 1 {
			src.fp = "v2"
		}
	}
	r := newFakeReloader(src, specSecrets())
	mustReload(t, r, true)
	if src.sets != 2 {
		t.Fatalf("%d reads; want the first read repeated once", src.sets)
	}
	// The kept fingerprint is the retry's, so a poll that finds the source as the retry read it rebuilds nothing.
	rev := r.Revision(t.Context())
	mustReload(t, r, false)
	if src.sets != 2 || r.Revision(t.Context()) != rev {
		t.Errorf("poll after the retry: %d reads, revision moved %v", src.sets, r.Revision(t.Context()) != rev)
	}

	moving := &fakeSource{t: t, fp: "a"}
	moving.onSet = func(int) { moving.fp += "+" }
	r = newFakeReloader(moving, specSecrets())
	if err := r.reload(t.Context(), true); !errors.Is(err, errConfigMoving) || r.Current() != nil || r.Status().Errors != 0 || r.Status().Stale {
		t.Errorf("moving source: err %v, snapshot %v, status %+v", err, r.Current(), r.Status())
	}
	moving.onSet = nil
	mustReload(t, r, false)
	if r.Current() == nil {
		t.Error("nothing published once the source held still")
	}
}

// errHookCtx runs a hook on each Err call. Load checks ctx before each file, so the hook is a
// way to act on the directory while it is being loaded.
type errHookCtx struct {
	context.Context
	hook func(call int)
	n    int
}

func (c *errHookCtx) Err() error {
	c.n++
	c.hook(c.n)
	return c.Context.Err()
}

// A plain directory (no ..data) edited while it loads is read again, and the snapshot is the
// edited config. The fingerprint is taken with the read, not after it: a fingerprint taken
// after Load would match the recheck and publish files read before the edit under the new one.
func TestReloaderReadsAPlainDirectoryEditedDuringTheLoad(t *testing.T) {
	dir := specDir(t)
	path := filepath.Join(dir, "binding.yaml")
	edited := strings.Replace(mustRead(t, path), "C0123456789", "C0999999999", 1)
	ctx := &errHookCtx{Context: t.Context()}
	ctx.hook = func(call int) {
		if call == 4 { // binding.yaml has been read, chatconnection.yaml has not
			writeFileAtomic(t, path, edited)
		}
	}
	r := newTestReloader(dir, specSecrets())
	if err := r.reload(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Current().BindingForChannel("agent-kourier-system", "slack-main", "C0999999999"); !ok {
		t.Error("the edit made during the load was not published")
	}
}

// The generation is part of the fingerprint: two generations with the same file names, sizes and
// modification times differ only in where ..data points, and that must move the fingerprint.
func TestDirSourceFingerprintFollowsTheGeneration(t *testing.T) {
	binding := mustRead(t, "../../api/v1alpha1/testdata/binding.yaml")
	same := strings.Replace(binding, "C0123456789", "C0999999999", 1) // same size
	dir := mountDir(t, map[string]map[string]string{
		"one": {"binding.yaml": binding},
		"two": {"binding.yaml": same},
	}, "one")
	when := time.Unix(1700000000, 0)
	for _, gen := range []string{"one", "two"} {
		if err := os.Chtimes(filepath.Join(dir, ".."+gen, "binding.yaml"), when, when); err != nil {
			t.Fatal(err)
		}
	}
	src := dirSource{dir}
	before := src.Fingerprint(t.Context())
	swapGeneration(t, dir, "two")
	if after := src.Fingerprint(t.Context()); after == before {
		t.Errorf("fingerprint %q did not move with ..data", after)
	}
}

// Set reads the generation ..data names, not the top-level links: a key the kubelet has
// added to the generation but not linked yet, or one it has unlinked, is read through ..data.
func TestDirSourceReadsTheGenerationNotTheLinks(t *testing.T) {
	dir := mountDir(t, twoGenerations(t), "one")
	if err := os.Remove(filepath.Join(dir, "binding.yaml")); err != nil {
		t.Fatal(err)
	}
	set, _, err := dirSource{dir}.Set(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Bindings) != 2 {
		t.Errorf("%d Bindings; want the generation's 2, including the one whose link is gone", len(set.Bindings))
	}
}
