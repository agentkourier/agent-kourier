package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/broker"
)

// A config that does not load at start exits 2 when the operator must fix it, since a restart finds the same config,
// and 4 when what failed may pass on a restart: the Secret caches did not sync, or the directory could not be read.
func TestExitCodeOfAConfigThatDoesNotLoad(t *testing.T) {
	secretOf := func(namespace, name string, keys ...string) *corev1.Secret {
		data := map[string][]byte{}
		for _, k := range keys {
			data[k] = []byte("fake-value")
		}
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Data: data}
	}
	slackMain := secretOf("agent-kourier-system", "slack-main", "botToken", "appToken")
	oidc := secretOf("sre", "agent-kourier-sre-alerts-oidc", "token")
	// example copies spec 7's example config, which refers to slackMain and oidc.
	example := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.CopyFS(dir, os.DirFS("../../api/v1alpha1/testdata")); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	// edit is the example with the first from in binding.yaml replaced by to.
	edit := func(from, to string) func(t *testing.T) string {
		return func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(example(t), "binding.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), from) {
				t.Fatalf("binding.yaml has no %q", from)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(data), from, to, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			return filepath.Dir(path)
		}
	}

	tests := []struct {
		name   string
		dir    func(t *testing.T) string
		client func() *fake.Clientset
		want   int
	}{
		{"a misspelt Binding field", edit("channel:", "chanel:"),
			func() *fake.Clientset { return fake.NewClientset(slackMain, oidc) }, exitUsage},
		{"a reference to no ChatConnection", edit("name: slack-main }", "name: slack }"),
			func() *fake.Clientset { return fake.NewClientset(slackMain, oidc) }, exitUsage},
		{"a missing Secret", example, func() *fake.Clientset { return fake.NewClientset(slackMain) }, exitUsage},
		{"a missing key", example, func() *fake.Clientset {
			return fake.NewClientset(secretOf("agent-kourier-system", "slack-main", "appToken"), oidc)
		}, exitUsage},
		{"Secret caches that do not sync", example, func() *fake.Clientset {
			c := fake.NewClientset(slackMain, oidc)
			c.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewInternalError(errors.New("etcd is unavailable"))
			})
			return c
		}, exitFailure},
		{"a directory that cannot be read", func(t *testing.T) string {
			if os.Geteuid() == 0 {
				t.Skip("root reads a directory whatever its mode")
			}
			dir := example(t)
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			return dir
		}, func() *fake.Clientset { return fake.NewClientset(slackMain, oidc) }, exitFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := broker.Bootstrap(t.Context(), tc.client(), broker.BootstrapOptions{
				Dir: tc.dir(t), SyncTimeout: 500 * time.Millisecond, Logger: slog.New(slog.DiscardHandler),
			})
			if err == nil {
				c.Stop()
				t.Fatal("Bootstrap succeeded")
			}
			if got := exitCode(err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", err, got, tc.want)
			}
		})
	}
}
