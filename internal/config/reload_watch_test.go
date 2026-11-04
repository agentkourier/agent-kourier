package config

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// watchingSecrets is a SecretResolver that is also a SecretWatcher, and records what the Reloader asks of it, in order.
type watchingSecrets struct {
	fakeSecrets
	hold     bool   // Watch waits for its context to end, as one that cannot reach the API server does
	onRetain func() // called by Retain, before it records

	mu    sync.Mutex
	calls []string
}

func (w *watchingSecrets) record(call string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, call)
}

func (w *watchingSecrets) Keys(ctx context.Context, namespace, name string) ([]string, error) {
	w.record("keys")
	return w.fakeSecrets.Keys(ctx, namespace, name)
}

func (w *watchingSecrets) Watch(ctx context.Context, names []SecretName) error {
	w.record(fmt.Sprint("watch ", names))
	if w.hold {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (w *watchingSecrets) Retain(names []SecretName) {
	if w.onRetain != nil {
		w.onRetain()
	}
	w.record(fmt.Sprint("retain ", names))
}

// take returns what was asked since the last call.
func (w *watchingSecrets) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	calls := slices.Clone(w.calls)
	w.calls = nil
	return calls
}

// A SecretWatcher is asked for the Secrets of a config before they are checked, and told to keep exactly the Secrets of the
// snapshot that a successful reload publishes. A reload that fails keeps the last good snapshot, so it must not make the
// resolver let go of that snapshot's Secrets.
func TestReloaderFollowsTheSecretsOfEachConfig(t *testing.T) {
	names := fmt.Sprint([]SecretName{{"agent-kourier-system", "slack-main"}, {"sre", "agent-kourier-sre-alerts-oidc"}})
	dir := specDir(t)
	path := filepath.Join(dir, "binding.yaml")
	good := mustRead(t, path)
	secrets := &watchingSecrets{fakeSecrets: specSecrets()}
	r := newTestReloader(dir, secrets)

	mustReload(t, r, true)
	calls := secrets.take()
	if len(calls) < 3 || calls[0] != "watch "+names || calls[len(calls)-1] != "retain "+names {
		t.Fatalf("a good reload asked %v; want watch, the checks, then retain, each with %s", calls, names)
	}
	for _, c := range calls[1 : len(calls)-1] {
		if c != "keys" {
			t.Errorf("between watch and retain the Reloader asked %q", c)
		}
	}

	writeFile(t, path, "kind: [") // does not load, so it names no Secrets
	if err := r.reload(context.Background(), false); err == nil {
		t.Error("a config that does not load was published")
	}
	if got := secrets.take(); len(got) != 0 {
		t.Errorf("a config that does not load: asked %v; want nothing", got)
	}

	writeFile(t, path, good)
	delete(secrets.fakeSecrets, "sre/agent-kourier-sre-alerts-oidc") // loads, but does not resolve
	if err := r.reload(context.Background(), true); err == nil {
		t.Fatal("a config with a missing Secret was published")
	}
	got := secrets.take()
	if len(got) == 0 || got[0] != "watch "+names {
		t.Errorf("a config that does not resolve: asked %v; want it to start with watch %s", got, names)
	}
	for _, c := range got {
		if strings.HasPrefix(c, "retain") {
			t.Errorf("a failed reload told the resolver to %s", c)
		}
	}
}

// Retain comes after the new snapshot is published, not before: a reader that finds a Secret dropped must find the snapshot
// that dropped it, or it could read the old snapshot's Binding for a Secret that is already let go.
func TestReloaderRetainsAfterItPublishes(t *testing.T) {
	secrets := &watchingSecrets{fakeSecrets: specSecrets()}
	r := newTestReloader(specDir(t), secrets)
	mustReload(t, r, true)
	old := r.Current()
	var atRetain *Snapshot
	secrets.onRetain = func() { atRetain = r.Current() }

	mustReload(t, r, true) // builds a new snapshot of the same config

	if atRetain == nil || atRetain == old || atRetain != r.Current() {
		t.Errorf("at Retain, Current was the old snapshot %t (nil %t); want the one the reload published", atRetain == old, atRetain == nil)
	}
}

// A reload does not wait for a Secret longer than SecretsWait: the check that follows reports the Secret, and the reload
// fails and keeps the last good snapshot.
func TestReloaderBoundsTheWaitForSecrets(t *testing.T) {
	secrets := &watchingSecrets{fakeSecrets: specSecrets()}
	r := NewReloader(ReloaderConfig{Dir: specDir(t), Secrets: secrets, SecretsWait: 20 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	mustReload(t, r, true)
	kept := r.Current()

	secrets.hold = true
	delete(secrets.fakeSecrets, "sre/agent-kourier-sre-alerts-oidc")
	begin := time.Now()
	err := r.reload(context.Background(), true)

	if err == nil || r.Current() != kept || !r.Status().Stale {
		t.Errorf("reload = %v, kept %t, status %+v; want it to fail, keep the snapshot and be stale", err, r.Current() == kept, r.Status())
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("the reload took %s; want it to stop waiting after SecretsWait", took)
	}
}
