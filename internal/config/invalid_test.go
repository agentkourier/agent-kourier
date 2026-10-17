package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ErrInvalid marks what the operator must fix in the config, and only that: a directory or file that cannot be read,
// a Secret cache that cannot be read and a cancellation are not the config's fault.
func TestErrInvalidMarksOnlyTheConfigsFaults(t *testing.T) {
	unreadable := func(t *testing.T, path string) {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its mode")
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
	}
	load := func(t *testing.T, dir string) error {
		t.Helper()
		_, err := Load(context.Background(), dir)
		return err
	}
	resolve := func(t *testing.T, mutate func(*Set, fakeSecrets), secrets func(fakeSecrets) SecretResolver) error {
		t.Helper()
		set, f := specSet(t), specSecrets()
		mutate(set, f)
		_, err := Resolve(context.Background(), set, secrets(f))
		return err
	}
	same := func(f fakeSecrets) SecretResolver { return f }
	keep := func(*Set, fakeSecrets) {}
	tests := []struct {
		name        string
		err         func(t *testing.T) error
		wantInvalid bool
	}{
		{"a misspelt field", func(t *testing.T) error {
			return load(t, write(t, map[string]string{"b.yaml": strings.Replace(bindingDoc, "channel:", "chanel:", 1)}))
		}, true},
		{"a YAML syntax error", func(t *testing.T) error {
			return load(t, write(t, map[string]string{"b.yaml": "kind: [Binding\n"}))
		}, true},
		{"a directory that does not exist", func(t *testing.T) error {
			return load(t, filepath.Join(t.TempDir(), "absent"))
		}, true},
		{"a directory that cannot be read", func(t *testing.T) error {
			dir := write(t, map[string]string{"b.yaml": bindingDoc})
			unreadable(t, dir)
			return load(t, dir)
		}, false},
		{"a file that cannot be read", func(t *testing.T) error {
			dir := write(t, map[string]string{"b.yaml": bindingDoc})
			unreadable(t, filepath.Join(dir, "b.yaml"))
			return load(t, dir)
		}, false},
		{"a missing Secret", func(t *testing.T) error {
			return resolve(t, func(_ *Set, f fakeSecrets) { delete(f, "sre/agent-kourier-sre-alerts-oidc") }, same)
		}, true},
		{"a missing key", func(t *testing.T) error {
			return resolve(t, func(_ *Set, f fakeSecrets) { f["agent-kourier-system/slack-main"] = []string{"appToken"} }, same)
		}, true},
		{"a reference to no ChatConnection", func(t *testing.T) error {
			return resolve(t, func(s *Set, _ fakeSecrets) { sreAlerts(s).Spec.Chat.ConnectionRef.Name = "slack" }, same)
		}, true},
		{"a Secret that cannot be read", func(t *testing.T) error {
			return resolve(t, keep, func(fakeSecrets) SecretResolver { return failingSecrets{} })
		}, false},
		{"a cancelled load", func(t *testing.T) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := Load(ctx, write(t, map[string]string{"b.yaml": bindingDoc}))
			return err
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			if err == nil {
				t.Fatal("got no error")
			}
			if got := errors.Is(err, ErrInvalid); got != tc.wantInvalid {
				t.Errorf("errors.Is(%v, ErrInvalid) = %t, want %t", err, got, tc.wantInvalid)
			}
		})
	}
}
