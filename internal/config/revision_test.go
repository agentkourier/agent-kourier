package config

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// versionedSecrets is a SecretResolver that also has versions, like the cluster-backed one.
// A Secret in versions but not in the embedded keys is missing, and one in broken is unreadable.
type versionedSecrets struct {
	fakeSecrets
	versions map[string]string
	broken   map[string]bool
}

func newVersionedSecrets() *versionedSecrets {
	return &versionedSecrets{
		fakeSecrets: specSecrets(),
		versions:    map[string]string{"agent-kourier-system/slack-main": "11", "sre/agent-kourier-sre-alerts-oidc": "22"},
		broken:      map[string]bool{},
	}
}

func (v *versionedSecrets) SecretVersion(ctx context.Context, namespace, name string) (string, error) {
	id := namespace + "/" + name
	if v.broken[id] {
		return "", errors.New("cache not synced")
	}
	if _, err := v.Keys(ctx, namespace, name); err != nil {
		return "", err
	}
	return v.versions[id], nil
}

// loadedReloader loads a fresh copy of spec 7's examples with secrets and returns the Reloader and the directory.
func loadedReloader(t *testing.T, secrets SecretResolver) (*Reloader, string) {
	t.Helper()
	dir := specDir(t)
	r := newTestReloader(dir, secrets)
	mustReload(t, r, true)
	return r, dir
}

func TestRevisionKeepsAcrossRecheckThatFindsNothing(t *testing.T) {
	r, _ := loadedReloader(t, newVersionedSecrets())
	first, rev := r.Current(), r.Revision(t.Context())
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(rev) {
		t.Fatalf("revision = %q; want a hex digest", rev)
	}
	mustReload(t, r, true) // the one-minute recheck: a new snapshot, nothing new in it
	if r.Current() == first {
		t.Fatal("the recheck did not publish a new snapshot, so this test proves nothing")
	}
	if got := r.Revision(t.Context()); got != rev {
		t.Errorf("revision after a recheck = %q, want %q", got, rev)
	}
}

func TestRevisionChangesWithConfigFile(t *testing.T) {
	r, dir := loadedReloader(t, newVersionedSecrets())
	rev := r.Revision(t.Context())
	path := filepath.Join(dir, "binding.yaml")
	writeFile(t, path, mustRead(t, path)+"\n# edited\n")
	mustReload(t, r, false)
	edited := r.Revision(t.Context())
	if edited == rev {
		t.Error("revision did not change when a config file did")
	}
	mustReload(t, r, true)
	if got := r.Revision(t.Context()); got != edited {
		t.Errorf("revision after a recheck = %q, want %q", got, edited)
	}
}

func TestRevisionChangesWithSecretVersion(t *testing.T) {
	secrets := newVersionedSecrets()
	r, _ := loadedReloader(t, secrets)
	rev := r.Revision(t.Context())
	secrets.versions["sre/agent-kourier-sre-alerts-oidc"] = "23" // a rotation: same keys, new version, no reload
	rotated := r.Revision(t.Context())
	if rotated == rev {
		t.Error("revision did not change when a Secret's version did")
	}
	secrets.versions["sre/agent-kourier-sre-alerts-oidc"] = "22"
	if got := r.Revision(t.Context()); got != rev {
		t.Errorf("revision with the old version back = %q, want %q", got, rev)
	}
}

func TestRevisionTellsMissingFromUnreadableFromPresent(t *testing.T) {
	secrets := newVersionedSecrets()
	r, _ := loadedReloader(t, secrets)
	const id = "sre/agent-kourier-sre-alerts-oidc"
	present := r.Revision(t.Context())

	saved := secrets.fakeSecrets[id]
	delete(secrets.fakeSecrets, id)
	missing := r.Revision(t.Context())
	if got := r.Revision(t.Context()); got != missing {
		t.Errorf("a missing Secret gave %q then %q; want a stable value", missing, got)
	}

	secrets.fakeSecrets[id] = saved
	secrets.broken[id] = true
	unreadable := r.Revision(t.Context())
	if got := r.Revision(t.Context()); got != unreadable {
		t.Errorf("an unreadable Secret gave %q then %q; want a stable value", unreadable, got)
	}

	delete(secrets.broken, id)
	secrets.versions[id] = "24" // restored under a new version
	restored := r.Revision(t.Context())
	seen := map[string]string{present: "present", missing: "missing", unreadable: "unreadable", restored: "restored"}
	if len(seen) != 4 {
		t.Errorf("present, missing, unreadable and restored are not four distinct revisions: %v", seen)
	}
}

func TestRevisionIgnoresOrderAndRepeatsOfRefs(t *testing.T) {
	set := specSet(t)
	refs := set.secretRefs()
	if len(refs) < 2 {
		t.Fatalf("spec 7's examples have %d Secret refs; the test needs at least two", len(refs))
	}
	rev := func(refs []secretRef) string {
		r := newTestReloader(t.TempDir(), newVersionedSecrets())
		r.snap.Store(newPublished(&Snapshot{}, "fp", refs))
		return r.Revision(t.Context())
	}
	want := rev(refs)
	reversed := slices.Clone(refs)
	slices.Reverse(reversed)
	if got := rev(reversed); got != want {
		t.Errorf("reversed refs changed the revision: %q, want %q", got, want)
	}
	if got := rev(append(slices.Clone(refs), refs...)); got != want {
		t.Errorf("repeated refs changed the revision: %q, want %q", got, want)
	}
	if got := rev(refs[1:]); got == want {
		t.Error("a Secret fewer did not change the revision")
	}
}

func TestRevisionWithoutVersionsCoversTheConfigAlone(t *testing.T) {
	secrets := specSecrets() // fakeSecrets has no SecretVersion
	if _, ok := any(secrets).(SecretVersioner); ok {
		t.Fatal("fakeSecrets has versions; the test needs a resolver without")
	}
	r, dir := loadedReloader(t, secrets)
	rev := r.Revision(t.Context())
	mustReload(t, r, true)
	if got := r.Revision(t.Context()); got != rev {
		t.Errorf("revision after a recheck = %q, want %q", got, rev)
	}
	secrets["sre/agent-kourier-sre-alerts-oidc"] = []string{"token", "other"}
	if got := r.Revision(t.Context()); got != rev {
		t.Errorf("a Secret change moved a revision that has no Secret versions: %q, want %q", got, rev)
	}
	path := filepath.Join(dir, "binding.yaml")
	writeFile(t, path, mustRead(t, path)+"\n# edited\n")
	mustReload(t, r, false)
	if got := r.Revision(t.Context()); got == rev {
		t.Error("revision did not change when a config file did")
	}
}

func TestRevisionBeforeFirstLoad(t *testing.T) {
	a := newTestReloader(specDir(t), newVersionedSecrets())
	rev := a.Revision(t.Context())
	if rev == "" || rev != a.Revision(t.Context()) || rev != newTestReloader(t.TempDir(), nil).Revision(t.Context()) {
		t.Errorf("revision before the first load = %q; want one stable value", rev)
	}
	mustReload(t, a, true)
	if a.Revision(t.Context()) == rev {
		t.Error("loading did not change the revision")
	}
}

func TestRevisionDescribesTheLastGoodSnapshotAfterAFailedReload(t *testing.T) {
	r, dir := loadedReloader(t, newVersionedSecrets())
	rev := r.Revision(t.Context())
	path := filepath.Join(dir, "binding.yaml")
	good := mustRead(t, path)

	writeFile(t, path, "kind: [")
	if err := r.reload(context.Background(), false); err == nil {
		t.Fatal("a bad edit loaded")
	}
	if got := r.Revision(t.Context()); got != rev {
		t.Errorf("revision after a failed reload = %q, want %q", got, rev)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.reload(ctx, true)
	if got := r.Revision(t.Context()); got != rev {
		t.Errorf("revision after an interrupted reload = %q, want %q", got, rev)
	}

	writeFile(t, path, good+"\n# fixed differently\n")
	mustReload(t, r, false)
	if got := r.Revision(t.Context()); got == rev {
		t.Error("revision did not change once a good edit loaded")
	}
}

// Revision runs on a prober's goroutine while the loop reloads; -race proves the fingerprint is
// handed over safely. Each edit must also be visible in the revision once its reload returns.
func TestRevisionRunsConcurrentlyWithReloads(t *testing.T) {
	r, dir := loadedReloader(t, newVersionedSecrets())
	path := filepath.Join(dir, "binding.yaml")
	base := mustRead(t, path)

	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					_ = r.Revision(context.Background())
					_ = r.Current()
				}
			}
		})
	}
	seen := map[string]bool{r.Revision(t.Context()): true}
	for i := range 30 {
		writeFile(t, path, base+"\n# edit "+strings.Repeat("x", i+1)+"\n") // each longer, so a coarse clock cannot hide one
		mustReload(t, r, i%2 == 0)
		rev := r.Revision(t.Context())
		if seen[rev] {
			t.Errorf("edit %d: revision %q was already seen", i, rev)
		}
		seen[rev] = true
	}
	close(done)
	readers.Wait()
}
