package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// canary is planted in every Secret value; it must never come back out of a formatted value,
// an error, a log line or an audit row.
const canary = "canary-9f3a-value-do-not-print"

var _ agent.CredentialsFunc = (*Source)(nil).Credentials

// fixture is a Binding in "sre" that reaches an AgentBackend in "platform".
type fixture struct {
	binding *v1alpha1.Binding
	backend *v1alpha1.AgentBackend
	reader  *fakeReader
}

func newFixture(id v1alpha1.Identity, headers ...v1alpha1.Header) *fixture {
	return &fixture{
		binding: &v1alpha1.Binding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "oncall"},
			Spec: v1alpha1.BindingSpec{
				Agent:    v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "platform", Name: "kagent"}, Name: "ask", Namespace: "kagent"},
				Identity: id,
			},
		},
		backend: &v1alpha1.AgentBackend{
			ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "kagent"},
			Spec:       v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: "http://127.0.0.1:1", Headers: headers},
		},
		reader: newReader(map[string]string{}),
	}
}

func (f *fixture) source() *Source { return New(f.binding, f.backend, f.reader) }

func tokenRef() *v1alpha1.SecretKeyRef { return &v1alpha1.SecretKeyRef{Name: "creds"} }

func TestCredentials(t *testing.T) {
	secretHeader := v1alpha1.Header{Name: "X-Api-Key", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "gw", Key: "key"}}
	plainHeader := v1alpha1.Header{Name: "X-Team", Value: "sre"}
	vals := map[string]string{"sre/creds/token": "tok", "platform/gw/key": "gwkey"}
	for _, tc := range []struct {
		name    string
		id      v1alpha1.Identity
		headers []v1alpha1.Header
		want    dialect.Credentials
	}{
		{"token, read in the Binding's namespace", v1alpha1.Identity{TokenSecretRef: tokenRef()}, nil, dialect.Credentials{Token: "tok"}},
		{"user ID only", v1alpha1.Identity{UserID: "svc"}, nil, dialect.Credentials{UserID: "svc"}},
		{"token and user ID", v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()}, nil, dialect.Credentials{UserID: "svc", Token: "tok"}},
		{
			"headers, the secret one read in the backend's namespace",
			v1alpha1.Identity{TokenSecretRef: tokenRef()}, []v1alpha1.Header{secretHeader, plainHeader},
			dialect.Credentials{Token: "tok", Headers: http.Header{"X-Api-Key": {"gwkey"}, "X-Team": {"sre"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(tc.id, tc.headers...)
			f.reader.vals = vals
			got, err := f.source().Credentials(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got.Token != tc.want.Token || got.UserID != tc.want.UserID || fmt.Sprint(got.Headers) != fmt.Sprint(tc.want.Headers) {
				t.Errorf("Credentials mismatch: token %q userID %q headers %v; want token %q userID %q headers %v",
					got.Token, got.UserID, got.Headers, tc.want.Token, tc.want.UserID, tc.want.Headers)
			}
		})
	}
}

func TestTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   v1alpha1.Identity
		want bool
	}{
		{"a token secret makes the token required", v1alpha1.Identity{TokenSecretRef: tokenRef()}, true},
		{"token and user ID: still required", v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()}, true},
		{"user ID only", v1alpha1.Identity{UserID: "svc"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(tc.id)
			s := f.source()
			got := s.Target()
			if got.TokenRequired != tc.want || s.TokenRequired() != tc.want {
				t.Errorf("TokenRequired = %v, want %v", got.TokenRequired, tc.want)
			}
			if got.Binding != "sre/oncall" || got.Backend != f.backend || got.Agent != f.binding.Spec.Agent || got.Credentials == nil {
				t.Errorf("Target = %+v, want the Binding's key, backend and agent, and a credentials func", got)
			}
		})
	}
}

// spyDialect counts connects; a Pool over it reveals whether a call got as far as a backend.
type spyDialect struct {
	dialect.Dialect
	connects atomic.Int32
}

func (d *spyDialect) Connect(context.Context, *v1alpha1.AgentBackend, v1alpha1.AgentRef, dialect.Credentials) (*a2aclient.Client, error) {
	d.connects.Add(1)
	return nil, errors.New("spy: no backend")
}

// TestUnreadableSecretFailsTheTurn: a Binding with a userId and an unreadable token secret
// gets an error and empty credentials, and through the Pool no connection at all, so
// X-User-Id is never sent.
func TestUnreadableSecretFailsTheTurn(t *testing.T) {
	header := v1alpha1.Header{Name: "X-Api-Key", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "gw"}}
	for _, tc := range []struct {
		name string
		vals map[string]string
		want error
	}{
		{"token secret missing", map[string]string{"platform/gw/token": "k"}, ErrTokenUnreadable},
		{"header secret missing", map[string]string{"sre/creds/token": "t"}, ErrHeaderUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()}, header)
			f.reader.vals = tc.vals
			s := f.source()
			got, err := s.Credentials(t.Context())
			if !errors.Is(err, tc.want) || !errors.Is(err, secret.ErrKeyNotFound) {
				t.Fatalf("Credentials err = %v, want %v wrapping ErrKeyNotFound", err, tc.want)
			}
			if got.UserID != "" || got.Token != "" || got.Headers != nil {
				t.Error("a failed Credentials returned something usable; it must fall back to nothing")
			}

			spy := &spyDialect{}
			p := agent.NewPool(spy, agent.Config{})
			defer p.Close()
			err = p.Agent(s.Target()).Send(t.Context(), a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")), nil)
			if !errors.Is(err, tc.want) || !errors.Is(err, agent.ErrNotSent) {
				t.Errorf("Send err = %v, want ErrNotSent wrapping %v", err, tc.want)
			}
			if n := spy.connects.Load(); n != 0 {
				t.Errorf("%d connects after an unreadable secret, want 0", n)
			}
		})
	}
}

// watchRegistered closes the returned channel once the "sre" watch exists, because the fake
// tracker drops events for a watch that is not registered yet (as in the k8ssecrets tests).
func watchRegistered(client *fake.Clientset) <-chan struct{} {
	registered, once := make(chan struct{}), sync.Once{}
	client.PrependWatchReactor("secrets", func(a k8stesting.Action) (bool, watch.Interface, error) {
		w, err := client.Tracker().Watch(a.GetResource(), a.GetNamespace())
		if a.GetNamespace() == "sre" {
			once.Do(func() { close(registered) })
		}
		return true, w, err
	})
	return registered
}

// TestRotationIsPickedUpWithoutRestart runs against the real resolver's watch: the next
// Credentials call after the Secret changes carries the new token and header, and the
// credentials returned before are unchanged.
func TestRotationIsPickedUpWithoutRestart(t *testing.T) {
	sec := func(token, key string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "creds"},
			Data:       map[string][]byte{"token": []byte(token), "key": []byte(key)},
		}
	}
	client := fake.NewClientset(sec("old-token", "old-key"))
	registered := watchRegistered(client)
	r := k8ssecrets.New(client, []string{"sre"})
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-registered

	f := newFixture(v1alpha1.Identity{TokenSecretRef: tokenRef()}, v1alpha1.Header{Name: "X-Api-Key", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "creds", Key: "key"}})
	f.backend.Namespace = "sre" // header secrets live beside the backend
	s := New(f.binding, f.backend, r)
	before, err := s.Credentials(t.Context())
	if err != nil || before.Token != "old-token" || before.Headers.Get("X-Api-Key") != "old-key" {
		t.Fatalf("first Credentials = %v, %v", before.Token, err)
	}
	if _, err := client.CoreV1().Secrets("sre").Update(t.Context(), sec("new-token", "new-key"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := s.Credentials(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got.Token == "new-token" && got.Headers.Get("X-Api-Key") == "new-key" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotated Secret never reached Credentials")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if before.Token != "old-token" {
		t.Error("credentials returned earlier changed")
	}
}

// TestSecretValuesNeverPrinted plants a canary in the token and in a header, then formats
// and formats everything a caller can reach, on the success path and the failure paths.
func TestSecretValuesNeverPrinted(t *testing.T) {
	f := newFixture(v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()},
		v1alpha1.Header{Name: "X-Api-Key", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "gw"}})
	f.reader.vals = map[string]string{"sre/creds/token": canary, "platform/gw/token": canary}
	s := f.source()
	ctx := t.Context()

	var seen []string
	note := func(what string, v any) {
		seen = append(seen, fmt.Sprintf("%s: %v %+v %#v %s %q %x", what, v, v, v, v, v, v))
	}
	creds, err := s.Credentials(ctx)
	if err != nil || creds.Token != canary || creds.Headers.Get("X-Api-Key") != canary {
		t.Fatalf("Credentials = %v, %v", err, creds.Headers)
	}
	note("credentials", creds)
	note("source", s)
	note("target", s.Target())
	js, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	seen = append(seen, string(js))
	tok, err := NewStaticToken(f.reader, "sre", *tokenRef()).Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	note("token", tok)
	note("static token", NewStaticToken(f.reader, "sre", *tokenRef()))

	// Failure paths.
	f.reader.setErr(fmt.Errorf("upstream: %w", secret.ErrEmptyValue))
	_, err = s.Credentials(ctx)
	note("unreadable token", err)
	f.reader.setErr(nil)
	f.reader.set("sre/creds/token", canary)
	delete(f.reader.vals, "platform/gw/token")
	_, err = s.Credentials(ctx)
	note("unreadable header", err)

	for _, out := range seen {
		if strings.Contains(out, canary) {
			t.Errorf("the canary leaked: %s", out)
		}
	}
}
