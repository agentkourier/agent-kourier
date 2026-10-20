package config

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// specDir copies spec 7's examples into a fresh directory.
func specDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../api/v1alpha1/testdata")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// countingSecrets counts reads and can act on them.
type countingSecrets struct {
	fakeSecrets
	reads  int
	onRead func(reads int)
}

func (c *countingSecrets) Keys(ctx context.Context, namespace, name string) ([]string, error) {
	c.reads++
	if c.onRead != nil {
		c.onRead(c.reads)
	}
	return c.fakeSecrets.Keys(ctx, namespace, name)
}

func newTestReloader(dir string, secrets SecretResolver) *Reloader {
	return NewReloader(ReloaderConfig{Dir: dir, Secrets: secrets, Logger: slog.New(slog.DiscardHandler)})
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mustReload reloads and fails the test if that fails.
func mustReload(t *testing.T, r *Reloader, force bool) {
	t.Helper()
	if err := r.reload(context.Background(), force); err != nil {
		t.Fatal(err)
	}
}

func TestReloaderKeepsLastGoodSnapshot(t *testing.T) {
	dir := specDir(t)
	path := filepath.Join(dir, "binding.yaml")
	good := mustRead(t, path)
	r := newTestReloader(dir, specSecrets())
	mustReload(t, r, true)
	first, st := r.Current(), r.Status()
	if first == nil || st.LastSuccess.IsZero() || st.Stale || st.Errors != 0 {
		t.Fatalf("after a good load: snapshot %v, status %+v", first, st)
	}

	writeFile(t, path, "kind: [")
	if err := r.reload(context.Background(), false); err == nil {
		t.Fatal("a bad edit loaded")
	}
	if bad := r.Status(); r.Current() != first || !bad.Stale || bad.Errors != 1 || !bad.LastSuccess.Equal(st.LastSuccess) {
		t.Errorf("after a bad edit: snapshot kept %v, status %+v", r.Current() == first, bad)
	}
	if err := r.reload(context.Background(), false); err != nil || r.Status().Errors != 1 {
		t.Errorf("an unchanged bad file was loaded again: %v, %+v", err, r.Status())
	}

	writeFile(t, path, good)
	mustReload(t, r, false)
	fixed := r.Current()
	if st := r.Status(); fixed == first || st.Stale || st.Errors != 1 {
		t.Errorf("after the fix: new snapshot %v, status %+v", fixed != first, st)
	}

	// A reload cut short is "interrupted", not a config error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.reload(ctx, true); err == nil || r.Current() != fixed || r.Status().Errors != 1 || r.Status().Stale {
		t.Errorf("interrupted reload: err %v, status %+v", err, r.Status())
	}
}

// OnError counts each failed reload once, as Status.Errors does, and is not told of one that was
// only cut short.
func TestReloaderCallsOnErrorForEachCountedFailure(t *testing.T) {
	dir := specDir(t)
	path := filepath.Join(dir, "binding.yaml")
	var seen []error
	r := NewReloader(ReloaderConfig{Dir: dir, Secrets: specSecrets(), Logger: slog.New(slog.DiscardHandler), OnError: func(err error) { seen = append(seen, err) }})
	mustReload(t, r, true)

	writeFile(t, path, "kind: [")
	if err := r.reload(context.Background(), false); err == nil || len(seen) != 1 || !errors.Is(seen[0], err) {
		t.Fatalf("after a bad edit: err %v, hook saw %v; want that one error", err, seen)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.reload(ctx, true)
	if len(seen) != 1 {
		t.Errorf("an interrupted reload reached the hook: %v", seen)
	}
}

func TestReloaderRebuildsOnlyOnChangeButRechecksSecrets(t *testing.T) {
	secrets := &countingSecrets{fakeSecrets: specSecrets()}
	r := newTestReloader(specDir(t), secrets)
	mustReload(t, r, true)
	first, reads := r.Current(), secrets.reads
	mustReload(t, r, false)
	if r.Current() != first || secrets.reads != reads {
		t.Errorf("no change: rebuilt (%d more reads)", secrets.reads-reads)
	}

	mustReload(t, r, true)
	if r.Current() == first || secrets.reads == reads {
		t.Errorf("forced: not rebuilt (%d more reads)", secrets.reads-reads)
	}
	kept := r.Current()
	delete(secrets.fakeSecrets, "sre/agent-kourier-sre-alerts-oidc") // Secrets change without a file change
	if err := r.reload(context.Background(), true); err == nil || r.Current() != kept || r.Status().Errors != 1 {
		t.Errorf("a vanished Secret: err %v, status %+v", err, r.Status())
	}
}

// The approval warnings depend on the backend's dialect: the a2a dialect never posts an approval, so its Bindings are told
// that toolApprovals does nothing instead of who may decide.
func TestReloaderWarnsAboutApprovalPolicyByDialect(t *testing.T) {
	const (
		anyMember = "tool approvals are enabled without approver groups; any non-bot channel member may decide"
		noEffect  = "toolApprovals has no effect on the a2a dialect"
	)
	for _, tc := range []struct {
		name, dialect, interactions string
		want, notWant               []string
	}{
		{"kagent without groups", "kagent-v1", "    toolApprovals: true\n", []string{anyMember}, []string{noEffect}},
		{"kagent with groups", "kagent-v1", "    toolApprovals: true\n    approverGroups: [S0123456789]\n", nil, []string{anyMember, noEffect}},
		{"a2a without groups", "a2a", "    toolApprovals: true\n", []string{noEffect}, []string{anyMember}},
		{"a2a with groups", "a2a", "    toolApprovals: true\n    approverGroups: [S0123456789]\n", []string{noEffect}, []string{anyMember}},
		{"a2a without approvals", "a2a", "", nil, []string{anyMember, noEffect}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := specDir(t)
			path := filepath.Join(dir, "binding.yaml")
			writeFile(t, path, strings.Replace(mustRead(t, path), "    askUser: true\n", "    askUser: true\n"+tc.interactions, 1))
			path = filepath.Join(dir, "agentbackend.yaml")
			writeFile(t, path, strings.Replace(mustRead(t, path), "dialect: kagent-v1", "dialect: "+tc.dialect, 1))
			var logs bytes.Buffer
			r := NewReloader(ReloaderConfig{Dir: dir, Secrets: specSecrets(), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			mustReload(t, r, true)
			got := logs.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) || !strings.Contains(got, "binding=sre/sre-alerts") {
					t.Errorf("log = %q; want %q naming the Binding", got, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("log = %q; want no %q", got, w)
				}
			}
		})
	}
}

func TestReloaderWarnsApprovalsOnlyForInitialOrChangedConfig(t *testing.T) {
	dir := specDir(t)
	path := filepath.Join(dir, "binding.yaml")
	const warning = "tool approvals are enabled without approver groups; any non-bot channel member may decide"
	var logs bytes.Buffer
	r := NewReloader(ReloaderConfig{Dir: dir, Secrets: specSecrets(), Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	mustReload(t, r, true)
	beforeRecheck := logs.String()
	mustReload(t, r, true)
	if got := strings.TrimPrefix(logs.String(), beforeRecheck); strings.Contains(got, warning) {
		t.Errorf("unchanged forced recheck logged a policy warning: %q", got)
	}

	writeFile(t, path, strings.Replace(mustRead(t, path), "    askUser: true\n", "    askUser: true\n    toolApprovals: true\n", 1))
	beforeChange := logs.String()
	mustReload(t, r, false)
	if got := strings.TrimPrefix(logs.String(), beforeChange); !strings.Contains(got, warning) {
		t.Errorf("changed policy load logged %q; want the policy warning", got)
	}

	beforeRecheck = logs.String()
	mustReload(t, r, true)
	if got := strings.TrimPrefix(logs.String(), beforeRecheck); strings.Contains(got, warning) {
		t.Errorf("unchanged forced policy recheck logged a policy warning: %q", got)
	}
}

func TestReloaderStartFailsOnBadConfig(t *testing.T) {
	dir := specDir(t)
	writeFile(t, filepath.Join(dir, "binding.yaml"), "kind: [")
	r := newTestReloader(dir, specSecrets())
	if err := r.Start(context.Background()); err == nil || r.Current() != nil {
		t.Errorf("Start = %v, snapshot %v; want an error and no snapshot", err, r.Current())
	}
	r.Wait() // no loop was started
	goleak.VerifyNone(t)
}

func TestReloaderBrokenStateFailsOnceAndRecovers(t *testing.T) {
	dir := specDir(t)
	var logs bytes.Buffer
	r := NewReloader(ReloaderConfig{Dir: dir, Secrets: specSecrets(), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	mustReload(t, r, true)

	// A dangling link breaks Load and the fingerprint's Stat alike, and must not count on every poll.
	link := filepath.Join(dir, "x.yaml")
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		_ = r.reload(context.Background(), false)
	}
	if r.Status().Errors != 1 || strings.Count(logs.String(), "reload failed") != 1 {
		t.Errorf("errors %d over 4 polls of a broken directory; logs:\n%s", r.Status().Errors, logs.String())
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	mustReload(t, r, false)
	if st := r.Status(); st.Stale || !strings.Contains(logs.String(), "config reloaded after failed reloads") || !strings.Contains(logs.String(), "failed_reloads=1") {
		t.Errorf("after the fix: %+v; logs:\n%s", st, logs.String())
	}
	logged := logs.Len()
	mustReload(t, r, true) // a recheck that finds nothing new stays silent
	if logs.Len() != logged {
		t.Errorf("a no-change recheck logged: %s", logs.String()[logged:])
	}
}

// mountDir lays dir out like a mounted ConfigMap: each generation is a hidden directory of
// files, ..data links to the current one, and each file links through ..data.
func mountDir(t *testing.T, gens map[string]map[string]string, current string) string {
	t.Helper()
	dir := t.TempDir()
	for gen, files := range gens {
		if err := os.Mkdir(filepath.Join(dir, ".."+gen), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			writeFile(t, filepath.Join(dir, ".."+gen, name), content)
			if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
		}
	}
	swapGeneration(t, dir, current)
	return dir
}

// swapGeneration points ..data at a generation atomically, as the kubelet does.
func swapGeneration(t *testing.T, dir, gen string) {
	t.Helper()
	tmp := filepath.Join(dir, "..tmp")
	if err := os.Symlink(".."+gen, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
}

// twoGenerations returns spec 7's examples twice. In "two" the backend is renamed, in every
// file that mentions it, so files from different generations cannot resolve together.
func twoGenerations(t *testing.T) map[string]map[string]string {
	t.Helper()
	one, two := map[string]string{}, map[string]string{}
	for _, name := range []string{"agentbackend.yaml", "binding-minimal.yaml", "binding.yaml", "chatconnection.yaml"} {
		one[name] = mustRead(t, filepath.Join("../../api/v1alpha1/testdata", name))
		two[name] = strings.ReplaceAll(one[name], "kagent-pilot", "kagent-two")
	}
	return map[string]map[string]string{"one": one, "two": two}
}

func TestReloaderLoadsOneGeneration(t *testing.T) {
	dir := mountDir(t, twoGenerations(t), "one")

	// Once pinned, the directory keeps its generation's files after ..data moves on. Reading
	// through the top-level links instead would now give a mix that cannot resolve.
	root, _ := generation(dir)
	swapGeneration(t, dir, "two")
	if set, err := Load(context.Background(), root); err != nil || set.AgentBackends[0].Name != "kagent-pilot" {
		t.Errorf("pinned load = %+v, %v; want generation one", set, err)
	}

	// If ..data moves while a load resolves, the load goes again and the result is all "two".
	swapGeneration(t, dir, "one")
	secrets := &countingSecrets{fakeSecrets: specSecrets()}
	secrets.onRead = func(reads int) {
		if reads == 1 {
			swapGeneration(t, dir, "two")
		}
	}
	r := newTestReloader(dir, secrets)
	mustReload(t, r, true)
	if rb, ok := r.Current().Binding("sre", "sre-alerts"); !ok || rb.Backend.Name != "kagent-two" {
		t.Errorf("Binding = %+v, %v; want the generation that was current when the load ended", rb, ok)
	}
}

func TestReloaderLoop(t *testing.T) {
	dir := specDir(t)
	r := newTestReloader(dir, specSecrets())
	r.poll, r.secretsEvery = time.Millisecond, 0
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Readers use the current snapshot without locks while the loop reloads.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if _, ok := r.Current().Binding("sre", "sre-alerts"); !ok || r.Status().LastSuccess.IsZero() {
						t.Error("reader saw no snapshot")
						return
					}
				}
			}
		})
	}

	path := filepath.Join(dir, "binding.yaml")
	writeFileAtomic(t, path, strings.Replace(mustRead(t, path), "C0123456789", "C0999999999", 1))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, ok := r.Current().BindingForChannel("agent-kourier-system", "slack-main", "C0999999999"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop never published the edit")
		}
	}

	close(stop)
	readers.Wait()
	cancel()
	r.Wait()
	goleak.VerifyNone(t)
}

// writeFileAtomic keeps the reload loop from observing the truncate window of an in-place edit.
func writeFileAtomic(t *testing.T, path, contents string) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".reload-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.WriteString(contents); err != nil {
		_ = tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatal(err)
	}
}

func TestReloaderGivesUpOnAMovingConfig(t *testing.T) {
	dir := mountDir(t, twoGenerations(t), "one")
	secrets := &countingSecrets{fakeSecrets: specSecrets()}
	// Each attempt reads two Secrets, and the first of them moves the config to the other generation.
	secrets.onRead = func(reads int) {
		if reads%2 == 1 {
			swapGeneration(t, dir, []string{"two", "one"}[reads/2%2])
		}
	}
	r := newTestReloader(dir, secrets)
	if err := r.reload(context.Background(), true); !errors.Is(err, errConfigMoving) ||
		r.Current() != nil || r.Status().Errors != 0 || r.Status().Stale || r.lastFingerprint != "" {
		t.Errorf("moving config: err %v, snapshot %v, status %+v", err, r.Current(), r.Status())
	}
	secrets.onRead = nil
	mustReload(t, r, false) // the next poll rebuilds
	if r.Current() == nil {
		t.Error("nothing published once the config held still")
	}
}

func TestGenerationFollowsAbsoluteLink(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if root, _ := generation(dir); root != target {
		t.Errorf("generation root = %q, want %q", root, target)
	}
}
