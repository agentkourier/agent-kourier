package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

var _ session.Bindings = (*Bindings)(nil)

// jwt is an unsigned token with an email claim, which the stub reads as the owner.
func jwt(email string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"email":"`+email+`"}`)) + "."
}

// reader is a SecretReader whose values the test rotates.
type reader struct {
	mu   sync.Mutex
	vals map[string]string // "namespace/name/key"
}

func (r *reader) set(k, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals[k] = v
}

func (r *reader) Value(_ context.Context, ns, name, key string) (secret.Value, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return secret.New(r.vals[ns+"/"+name+"/"+key]), nil
}

// keys says every Secret has every key, as the snapshot builder only checks that they exist.
type keys struct{}

func (keys) Keys(context.Context, string, string) ([]string, error) {
	return []string{"token", "botToken", "appToken", "signingSecret"}, nil
}

// auditor counts the calls it is asked to record.
type auditor struct {
	mu    sync.Mutex
	calls []audit.Actor
}

func (a *auditor) AgentCall(_ context.Context, actor audit.Actor, _ audit.Call) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, actor)
	return nil
}

// snaps is a Snapshots whose snapshot the test swaps, as a reload does.
type snaps struct {
	p atomic.Pointer[config.Snapshot]
}

func (s *snaps) Current() *config.Snapshot { return s.p.Load() }

// snapshot resolves a config with one Binding, sre/oncall, on the backend at url, whose agent
// speaks dialect and keeps threads for ttl.
func snapshot(t *testing.T, url string, d v1alpha1.Dialect, ttl time.Duration) *config.Snapshot {
	t.Helper()
	be := v1alpha1.AgentBackend{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "kagent"},
		Spec:       v1alpha1.AgentBackendSpec{Dialect: d, URL: url, AllowedNamespaces: []string{"sre"}},
	}
	conn := v1alpha1.ChatConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "slack"},
		Spec: v1alpha1.ChatConnectionSpec{
			Platform: "slack", CredentialsSecretRef: v1alpha1.SecretRef{Name: "slack"}, AllowedNamespaces: []string{"sre"},
		},
	}
	b := v1alpha1.Binding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "oncall"},
		Spec: v1alpha1.BindingSpec{
			Agent:    v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "platform", Name: "kagent"}, Name: "test-agent", Namespace: "kagent"},
			Identity: v1alpha1.Identity{TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "creds"}},
			Chat:     v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Channel: "C1"},
			Session:  v1alpha1.Session{ThreadTTL: metav1.Duration{Duration: ttl}},
		},
	}
	set := &config.Set{AgentBackends: []v1alpha1.AgentBackend{be}, ChatConnections: []v1alpha1.ChatConnection{conn}, Bindings: []v1alpha1.Binding{b}}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// fixture is a Bindings over a stub agent that records the Authorization header of every request.
type fixture struct {
	b       *Bindings
	snaps   *snaps
	reader  *reader
	audit   *auditor
	url     string
	mu      sync.Mutex
	auths   []string
	metrics []string // the Binding of each OnRejected call
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	stub := stubagent.NewHandler()
	stub.Agent("kagent", "test-agent")
	f := &fixture{snaps: &snaps{}, reader: &reader{vals: map[string]string{"sre/creds/token": jwt("a@example.test")}}, audit: &auditor{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		stub.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(stub.Close)
	f.url = srv.URL
	f.snaps.p.Store(snapshot(t, srv.URL, v1alpha1.DialectKagentV1, 36*time.Hour))
	f.b = NewBindings(f.snaps, f.reader, f.audit, Options{OnRejected: func(b string) { f.metrics = append(f.metrics, b) }})
	t.Cleanup(f.b.Close)
	return f
}

// lastAuth is the Authorization header of the latest request the stub got.
func (f *fixture) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auths) == 0 {
		return ""
	}
	return f.auths[len(f.auths)-1]
}

func TestUnknownBindingWrapsTheSentinel(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"sre/nope", "nope/oncall", "oncall", "", "sre/oncall/extra"} {
		if _, err := f.b.Backend(name); !errors.Is(err, session.ErrUnknownBinding) {
			t.Errorf("Backend(%q) = %v, want an error wrapping ErrUnknownBinding", name, err)
		}
	}
}

func TestNoSnapshotYetIsNotAnUnknownBinding(t *testing.T) {
	f := newFixture(t)
	f.snaps.p.Store(nil)
	_, err := f.b.Backend("sre/oncall")
	if err == nil || errors.Is(err, session.ErrUnknownBinding) {
		t.Errorf("Backend before the first load = %v, want an error that is not ErrUnknownBinding", err)
	}
}

func TestBackendCarriesWhatTheSessionNeeds(t *testing.T) {
	f := newFixture(t)
	be, err := f.b.Backend("sre/oncall")
	if err != nil {
		t.Fatal(err)
	}
	if be.ThreadTTL != 36*time.Hour {
		t.Errorf("ThreadTTL = %s, want 36h", be.ThreadTTL)
	}
	if be.Identity != "secret:sre/creds" {
		t.Errorf("Identity = %q, want the token Secret", be.Identity)
	}
	if be.Agent == nil || be.Rejected == nil || be.Attribute == nil {
		t.Fatalf("Backend = %+v, want Agent, Rejected and Attribute set", be)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	be.Attribute(msg, dialect.User{ID: "U1", Name: "Ada"})
	if _, ok := msg.Metadata["agentcourier.dev/actor"]; !ok {
		t.Errorf("Attribute left no actor in %v: it is not the kagent-v1 dialect's", msg.Metadata)
	}
}

// Sessions hold a Backend for a whole turn, so a rotated token must reach the calls of one they
// already hold, as well as those of a new one.
func TestTokenRotationTakesEffectOnTheNextCall(t *testing.T) {
	f := newFixture(t)
	held, err := f.b.Backend("sre/oncall")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = held.Agent.GetTask(t.Context(), "task-1") // the task is unknown; the request is what counts
	if got, want := f.lastAuth(), "Bearer "+jwt("a@example.test"); got != want {
		t.Fatalf("first call sent %q, want the first token", got)
	}

	f.reader.set("sre/creds/token", jwt("b@example.test"))
	_, _ = held.Agent.GetTask(t.Context(), "task-2")
	if got, want := f.lastAuth(), "Bearer "+jwt("b@example.test"); got != want {
		t.Errorf("a held Backend sent %q after rotation, want the new token", got)
	}
	fresh, err := f.b.Backend("sre/oncall")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fresh.Agent.GetTask(t.Context(), "task-3")
	if got, want := f.lastAuth(), "Bearer "+jwt("b@example.test"); got != want {
		t.Errorf("a new Backend sent %q after rotation, want the new token", got)
	}
}

func TestRejectedIsWiredAndAuditedOnce(t *testing.T) {
	f := newFixture(t)
	be, err := f.b.Backend("sre/oncall")
	if err != nil {
		t.Fatal(err)
	}
	call := audit.Call{Op: audit.OpSend, MessageID: "m1", Err: agent.ErrCredentialsRejected}
	if err := be.Rejected(t.Context(), audit.Actor{User: "U1"}, call); err != nil {
		t.Fatal(err)
	}
	f.audit.mu.Lock()
	rows := append([]audit.Actor(nil), f.audit.calls...)
	f.audit.mu.Unlock()
	if len(rows) != 1 || rows[0].Binding != "sre/oncall" || rows[0].Identity != "secret:sre/creds" || rows[0].User != "U1" {
		t.Errorf("audited %+v, want one row for sre/oncall as secret:sre/creds", rows)
	}
	if len(f.metrics) != 1 || f.metrics[0] != "sre/oncall" {
		t.Errorf("OnRejected calls = %v, want one for sre/oncall", f.metrics)
	}

	call.Err = errors.New("something else")
	if err := be.Rejected(t.Context(), audit.Actor{}, call); err != nil || len(f.audit.calls) != 1 {
		t.Errorf("a call that was not rejected was audited or failed: %v, %d rows", err, len(f.audit.calls))
	}
}

func TestOnlyKagentV1IsBuilt(t *testing.T) {
	f := newFixture(t)
	f.snaps.p.Store(snapshot(t, f.url, v1alpha1.DialectA2A, time.Hour))
	_, err := f.b.Backend("sre/oncall")
	if err == nil || errors.Is(err, session.ErrUnknownBinding) || !strings.Contains(err.Error(), `"a2a"`) {
		t.Errorf("Backend of an a2a Binding = %v, want an error naming the dialect that is not the sentinel", err)
	}
}

func TestReloadChangesWhatBackendReturns(t *testing.T) {
	f := newFixture(t)
	f.snaps.p.Store(snapshot(t, f.url, v1alpha1.DialectKagentV1, 48*time.Hour))
	be, err := f.b.Backend("sre/oncall")
	if err != nil || be.ThreadTTL != 48*time.Hour {
		t.Fatalf("after a reload Backend = %+v, %v; want the new ThreadTTL of 48h", be, err)
	}

	f.snaps.p.Store(&config.Snapshot{}) // a reload that removed the Binding
	if _, err := f.b.Backend("sre/oncall"); !errors.Is(err, session.ErrUnknownBinding) {
		t.Errorf("after the Binding left the config Backend = %v, want ErrUnknownBinding", err)
	}
}
