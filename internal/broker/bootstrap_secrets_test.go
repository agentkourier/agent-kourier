package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

// listOf returns what a Secrets list asks for by name, as the informers of the resolver do.
func listOf(a k8stesting.Action) string {
	return a.(k8stesting.ListAction).GetListRestrictions().Fields.String()
}

// The Secrets of the config at start are waited for for as long as SyncTimeout, in Bootstrap, and the error says that the
// resolver did not start. Left to the Reloader they would be waited for for its own, shorter, bound, and the failure would
// read as a config that does not load.
func TestBootstrapWaitsForTheConfigsSecretsForTheSyncTimeout(t *testing.T) {
	client := cluster()
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if listOf(a) == "metadata.name=slack-main" {
			return true, nil, errors.New("connection refused") // the list never answers well
		}
		return false, nil, nil
	})
	opts := options(configDir(t))
	opts.SyncTimeout = 300 * time.Millisecond

	begin := time.Now()
	c, err := Bootstrap(t.Context(), client, opts)

	if c != nil {
		c.Stop()
		t.Fatal("Bootstrap succeeded")
	}
	if err == nil || !strings.Contains(err.Error(), "start the secret resolver") || !strings.Contains(err.Error(), "agent-kourier-system/slack-main") {
		t.Errorf("Bootstrap = %v, want an error from the secret resolver naming agent-kourier-system/slack-main", err)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("Bootstrap took %s, want it to end at its 300 ms sync timeout", took)
	}
}

// The wait at start is SyncTimeout's, not the Reloader's SecretsWait: a Secret that syncs after the Reloader's wait has
// ended, but within SyncTimeout, is read, and Bootstrap succeeds.
func TestBootstrapHonoursASyncTimeoutLongerThanTheReloadersWait(t *testing.T) {
	client := cluster()
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if listOf(a) == "metadata.name=slack-main" {
			time.Sleep(300 * time.Millisecond)
		}
		return false, nil, nil
	})
	opts := options(configDir(t))
	opts.SecretsWait = 50 * time.Millisecond
	opts.SyncTimeout = 5 * time.Second

	c, err := Bootstrap(t.Context(), client, opts)

	if err != nil {
		t.Fatalf("Bootstrap = %v, want the config to load once the slow Secret has synced", err)
	}
	c.Stop()
}

// What the last good snapshot reads stays readable through a reload that fails and does not name it: the failed config
// repoints the Binding's token to a Secret that does not exist, so the Secrets it names omit the one the snapshot uses. The
// Secret stays followed, and Revision, which reads its version, does not move.
func TestAFailedReloadThatRepointsASecretKeepsTheLastGoodSnapshotsSecret(t *testing.T) {
	dir := configDir(t)
	failures := make(chan error, 8)
	opts := options(dir)
	opts.OnReloadError = func(err error) { failures <- err }
	c, err := Bootstrap(t.Context(), cluster(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	revision := c.Reloader.Revision(t.Context())

	path := filepath.Join(dir, "binding.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	repointed := strings.Replace(string(data), "agent-kourier-sre-alerts-oidc", "no-such-token", 1)
	staging := filepath.Join(dir, ".binding.yaml.tmp")
	if err := os.WriteFile(staging, []byte(repointed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, path); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failures:
		if err == nil {
			t.Error("the hook was told of a nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reload never failed")
	}

	if _, err := c.Secrets.Keys(t.Context(), "sre", "agent-kourier-sre-alerts-oidc"); err != nil {
		t.Errorf("Keys of the last good snapshot's Secret after a failed reload = %v", err)
	}
	if got := c.Reloader.Revision(t.Context()); got != revision {
		t.Error("the revision moved across a failed reload: the last good snapshot's Secret was let go")
	}
}

// The Postgres DSN's Secret is followed from the start, before the store opens: a list of it that is forbidden ends Run in
// Bootstrap's resolver, with the denial, and not later when the store reads it.
func TestRunFollowsTheDSNSecretFromTheStart(t *testing.T) {
	c := postgresConfig(t, "postgres://x")
	c.Client.(*fake.Clientset).PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if listOf(a) != "metadata.name=agent-kourier-pg" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "agent-kourier-pg", errors.New("RBAC: no list"))
	})
	r := start(t, c)

	err := r.wait()

	if !errors.Is(err, k8ssecrets.ErrAccessDenied) || !strings.Contains(err.Error(), "start the secret resolver") {
		t.Errorf("Run = %v, want the denial of the DSN's Secret from the secret resolver Bootstrap starts", err)
	}
}
