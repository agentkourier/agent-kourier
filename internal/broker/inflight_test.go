package broker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/identity"
)

type nopAuditor struct{}

func (nopAuditor) AgentCall(context.Context, audit.Actor, audit.Call) error { return nil }

// A turn holds the Binding of the snapshot it began under. A reload that moves the Binding's token to another Secret drops
// the old Secret from the config, and the turn, which may run for as long as its stream retries or a pause lasts, must
// still read it: the credentials are those of the Binding it holds, not of the config now.
func TestAReloadDoesNotBreakTheCredentialsOfATurnInFlight(t *testing.T) {
	dir := configDir(t)
	client := cluster()
	if _, err := client.CoreV1().Secrets("sre").Create(t.Context(), secretOf("sre", "moved-token", "token"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := Bootstrap(t.Context(), client, options(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	before := c.Reloader.Current()
	rb, ok := before.Binding("sre", "sre-alerts")
	if !ok {
		t.Fatal("no sre/sre-alerts Binding")
	}
	src, err := identity.New(rb.Binding, rb.Backend, c.Secrets, nopAuditor{}, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Credentials(t.Context()); err != nil {
		t.Fatalf("Credentials before the reload: %v", err)
	}

	path := filepath.Join(dir, "binding.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(data), "agent-kourier-sre-alerts-oidc", "moved-token", 1)
	if moved == string(data) {
		t.Fatal("binding.yaml does not name the token Secret")
	}
	staging := filepath.Join(dir, ".binding.yaml.tmp") // a dot name is not a config file; the rename is atomic, as a ConfigMap mount's is
	if err := os.WriteFile(staging, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, path); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); c.Reloader.Current() == before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the reload never published; stale %t", c.Stale())
		}
	}
	// The window is real: the config no longer names the old Secret, so nothing follows it for the config.
	if _, err := c.Secrets.Keys(t.Context(), "sre", "agent-kourier-sre-alerts-oidc"); !errors.Is(err, k8ssecrets.ErrNotWatched) {
		t.Fatalf("Keys of the dropped Secret = %v, want ErrNotWatched", err)
	}

	creds, err := src.Credentials(t.Context())

	if err != nil || creds.Token.Reveal() != "fake-value" {
		t.Errorf("Credentials of the turn's Binding after the reload = %v; want its token still read", err)
	}
}
