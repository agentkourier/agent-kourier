package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var _ SecretReader = (*k8ssecrets.Resolver)(nil)

// fakeReader is a SecretReader over a map keyed "namespace/name/key". Like the real one it
// reads the default key for an empty key, and it counts reads.
type fakeReader struct {
	mu    sync.Mutex
	vals  map[string]string
	err   error // returned instead of any value when set
	reads []string
}

func newReader(vals map[string]string) *fakeReader { return &fakeReader{vals: vals} }

func (f *fakeReader) set(k, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[k] = v
}

func (f *fakeReader) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeReader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func (f *fakeReader) Value(_ context.Context, namespace, name, key string) (secret.Value, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key == "" {
		key = v1alpha1.DefaultSecretKey
	}
	id := namespace + "/" + name + "/" + key
	f.reads = append(f.reads, id)
	if f.err != nil {
		return secret.Value{}, f.err
	}
	v, ok := f.vals[id]
	if !ok {
		return secret.Value{}, fmt.Errorf("secret %s/%s key %q: %w", namespace, name, key, secret.ErrKeyNotFound)
	}
	return secret.New(v), nil
}

func TestStaticTokenReadsEveryCall(t *testing.T) {
	for _, tc := range []struct {
		name, key, want string
	}{
		{"default key", "", "first"},
		{"explicit key", "jwt", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader(map[string]string{"sre/creds/token": "first", "sre/creds/jwt": "other"})
			src := NewStaticToken(r, "sre", v1alpha1.SecretKeyRef{Name: "creds", Key: tc.key})
			v, err := src.Token(t.Context())
			if err != nil || v.Reveal() != tc.want {
				t.Fatalf("Token = %q, %v; want %q", v.Reveal(), err, tc.want)
			}
			r.set("sre/creds/token", "rotated")
			r.set("sre/creds/jwt", "rotated")
			if v, err := src.Token(t.Context()); err != nil || v.Reveal() != "rotated" {
				t.Errorf("after rotation Token = %q, %v; want rotated", v.Reveal(), err)
			}
			if got := r.count(); got != 2 {
				t.Errorf("%d reads for 2 calls, want one each: nothing may be cached", got)
			}
		})
	}
}

func TestStaticTokenErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    *fakeReader
		want error
	}{
		{"secret missing", &fakeReader{err: fmt.Errorf("secret sre/creds: %w", config.ErrSecretNotFound)}, config.ErrSecretNotFound},
		{"key missing", newReader(nil), secret.ErrKeyNotFound},
		{"empty value", &fakeReader{err: fmt.Errorf("key: %w", secret.ErrEmptyValue)}, secret.ErrEmptyValue},
		{"reader returns an empty value without an error", newReader(map[string]string{"sre/creds/token": ""}), secret.ErrEmptyValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewStaticToken(tc.r, "sre", v1alpha1.SecretKeyRef{Name: "creds"}).Token(t.Context())
			if !errors.Is(err, agent.ErrTokenUnreadable) || !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want agent.ErrTokenUnreadable wrapping %v", err, tc.want)
			}
			if v.Reveal() != "" {
				t.Error("a failed read returned a value")
			}
			for _, name := range []string{"sre", "creds", "token"} {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %q", err, name)
				}
			}
		})
	}
}

func TestStaticTokenRefreshReReads(t *testing.T) {
	r := newReader(map[string]string{"sre/creds/token": "t"})
	src := NewStaticToken(r, "sre", v1alpha1.SecretKeyRef{Name: "creds"})
	if err := src.Refresh(t.Context()); err != nil || r.count() != 1 {
		t.Errorf("Refresh = %v after %d reads, want nil after 1", err, r.count())
	}
	r.setErr(secret.ErrEmptyValue)
	if err := src.Refresh(t.Context()); !errors.Is(err, agent.ErrTokenUnreadable) {
		t.Errorf("Refresh of an unreadable token = %v, want agent.ErrTokenUnreadable", err)
	}
}
