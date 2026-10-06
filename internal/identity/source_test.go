package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// canary is planted in every Secret value; it must never come back out of a formatted value,
// an error, a log line or an audit row.
const canary = "canary-9f3a-value-do-not-print"

var _ agent.CredentialsFunc = (*Source)(nil).Credentials

// memAudit is a store.Audit in memory that can be made to fail.
type memAudit struct {
	mu   sync.Mutex
	rows []store.AuditEntry
	err  error
}

func (m *memAudit) AppendAudit(_ context.Context, e store.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.rows = append(m.rows, e)
	return nil
}

func (m *memAudit) ListAudit(context.Context, time.Time, int64, int) ([]store.AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.AuditEntry(nil), m.rows...), nil
}

func (m *memAudit) entries() []store.AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.AuditEntry(nil), m.rows...)
}

// fixture is a Binding in "sre" that reaches an AgentBackend in "platform".
type fixture struct {
	t       *testing.T
	binding *v1alpha1.Binding
	backend *v1alpha1.AgentBackend
	reader  *fakeReader
	store   *memAudit
	logs    *bytes.Buffer
	metric  []string // the Binding of each OnRejected call
}

func newFixture(t *testing.T, id v1alpha1.Identity, headers ...v1alpha1.Header) *fixture {
	return &fixture{
		t: t,
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
		store:  &memAudit{},
		logs:   &bytes.Buffer{},
	}
}

func (f *fixture) source() *Source {
	w := newWriter(f.t, f.store, audit.Options{Logger: slog.New(slog.NewTextHandler(f.logs, nil))})
	return mustNew(f.t, f.binding, f.backend, f.reader, w, Options{OnRejected: func(b string) { f.metric = append(f.metric, b) }})
}

// newWriter is audit.New over log with the failure meter left out: these tests are not about it.
func newWriter(t *testing.T, log store.Audit, opts audit.Options) *audit.Writer {
	t.Helper()
	opts.OnFailure = audit.NoFailure
	w, err := audit.New(log, opts)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func mustNew(t *testing.T, b *v1alpha1.Binding, be *v1alpha1.AgentBackend, r SecretReader, aud Auditor, opts Options) *Source {
	t.Helper()
	s, err := New(b, be, r, aud, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

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
		{"token, read in the Binding's namespace", v1alpha1.Identity{TokenSecretRef: tokenRef()}, nil, dialect.Credentials{Token: secret.New("tok")}},
		{"user ID only", v1alpha1.Identity{UserID: "svc"}, nil, dialect.Credentials{UserID: "svc"}},
		{"token and user ID", v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()}, nil, dialect.Credentials{UserID: "svc", Token: secret.New("tok")}},
		{
			"headers, the secret one read in the backend's namespace",
			v1alpha1.Identity{TokenSecretRef: tokenRef()}, []v1alpha1.Header{secretHeader, plainHeader},
			dialect.Credentials{Token: secret.New("tok"), Headers: dialect.Headers{"X-Api-Key": {secret.New("gwkey")}, "X-Team": {secret.New("sre")}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.id, tc.headers...)
			f.reader.vals = vals
			got, err := f.source().Credentials(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if g, w := plain(got), plain(tc.want); g != w {
				t.Errorf("Credentials = %s; want %s", g, w)
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
			f := newFixture(t, tc.id)
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
		name        string
		vals        map[string]string
		want, cause error
		names       string // the Secret the error must name
	}{
		{"token secret missing", map[string]string{"platform/gw/token": "k"}, agent.ErrTokenUnreadable, secret.ErrKeyNotFound, "sre/creds"},
		{"header secret missing", map[string]string{"sre/creds/token": "t"}, agent.ErrHeaderUnreadable, secret.ErrKeyNotFound, "platform/gw"},
		{"token with a trailing newline", map[string]string{"sre/creds/token": "t\n", "platform/gw/token": "k"}, agent.ErrTokenUnreadable, ErrControlCharacter, "sre/creds"},
		{"header with a trailing newline", map[string]string{"sre/creds/token": "t", "platform/gw/token": "k\n"}, agent.ErrHeaderUnreadable, ErrControlCharacter, "platform/gw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()}, header)
			f.reader.vals = tc.vals
			s := f.source()
			got, err := s.Credentials(t.Context())
			if !errors.Is(err, tc.want) || !errors.Is(err, tc.cause) {
				t.Fatalf("Credentials err = %v, want %v wrapping %v", err, tc.want, tc.cause)
			}
			if !strings.Contains(err.Error(), tc.names) || !strings.Contains(err.Error(), `"token"`) {
				t.Errorf("error %q does not name the Secret %s and its key", err, tc.names)
			}
			if got.UserID != "" || !got.Token.IsEmpty() || got.Headers != nil {
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

// TestEmptyTokenFromAnySourceFailsTheTurn: a TokenSource that returns "" without an error must
// not leave the userId as the identity.
func TestEmptyTokenFromAnySourceFailsTheTurn(t *testing.T) {
	f := newFixture(t, v1alpha1.Identity{UserID: "svc"})
	s := mustNew(t, f.binding, f.backend, f.reader, newWriter(t, f.store, audit.Options{}), Options{Token: &fakeToken{}})
	got, err := s.Credentials(t.Context())
	if !errors.Is(err, agent.ErrTokenUnreadable) || !errors.Is(err, secret.ErrEmptyValue) {
		t.Fatalf("Credentials err = %v, want ErrTokenUnreadable wrapping ErrEmptyValue", err)
	}
	if got.UserID != "" || !got.Token.IsEmpty() {
		t.Errorf("Credentials = %q and a token %v, want nothing usable", got.UserID, !got.Token.IsEmpty())
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

	f := newFixture(t, v1alpha1.Identity{TokenSecretRef: tokenRef()}, v1alpha1.Header{Name: "X-Api-Key", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "creds", Key: "key"}})
	f.backend.Namespace = "sre" // header secrets live beside the backend
	s := mustNew(t, f.binding, f.backend, r, newWriter(t, f.store, audit.Options{}), Options{})
	before, err := s.Credentials(t.Context())
	if err != nil || before.Token.Reveal() != "old-token" || header(before, "X-Api-Key") != "old-key" {
		t.Fatalf("first Credentials = %s, %v", plain(before), err)
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
		if got.Token.Reveal() == "new-token" && header(got, "X-Api-Key") == "new-key" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotated Secret never reached Credentials")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if before.Token.Reveal() != "old-token" {
		t.Error("credentials returned earlier changed")
	}
}

var errRejection = fmt.Errorf("send message: %w", errors.Join(agent.ErrNotSent, agent.ErrCredentialsRejected, &dialect.HTTPStatusError{Code: 401}))

func TestRejected(t *testing.T) {
	call := audit.Call{Op: audit.OpSend, ContextID: "ctx-1", MessageID: "msg-1", Err: errRejection}
	user := audit.Actor{User: "U1", Identity: "sa/investigator"}

	t.Run("re-reads, audits and counts", func(t *testing.T) {
		f := newFixture(t, v1alpha1.Identity{TokenSecretRef: tokenRef()})
		f.reader.vals = map[string]string{"sre/creds/token": "t"}
		if err := f.source().Rejected(t.Context(), user, call); err != nil {
			t.Fatal(err)
		}
		if got := f.reader.count(); got != 1 {
			t.Errorf("%d reads, want 1 re-read of the token Secret", got)
		}
		rows := f.store.entries()
		if len(rows) != 1 {
			t.Fatalf("%d audit rows, want 1: %+v", len(rows), rows)
		}
		want := store.AuditEntry{Kind: store.AuditAgentCall, User: "U1", Identity: "sa/investigator", Binding: "sre/oncall",
			Subject: "context=ctx-1 message=msg-1", Outcome: "send: failed credentials_rejected,not_sent,http_401"}
		got := rows[0]
		got.At = time.Time{}
		if got != want {
			t.Errorf("audit row = %+v, want %+v", got, want)
		}
		if len(f.metric) != 1 || f.metric[0] != "sre/oncall" {
			t.Errorf("OnRejected calls = %v, want one for sre/oncall", f.metric)
		}
	})

	t.Run("the row's identity defaults to something that is not the token", func(t *testing.T) {
		for _, tc := range []struct {
			id   v1alpha1.Identity
			want string
		}{
			{v1alpha1.Identity{TokenSecretRef: tokenRef()}, "secret:sre/creds"},
			{v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()}, "secret:sre/creds"}, // the userId is not sent
			{v1alpha1.Identity{UserID: "svc"}, "svc"},
		} {
			f := newFixture(t, tc.id)
			f.reader.vals = map[string]string{"sre/creds/token": "t"}
			if err := f.source().Rejected(t.Context(), audit.Actor{User: "U1"}, call); err != nil {
				t.Fatal(err)
			}
			if rows := f.store.entries(); len(rows) != 1 || rows[0].Identity != tc.want {
				t.Errorf("rows = %+v, want one with identity %q", rows, tc.want)
			}
		}
	})

	t.Run("anything else is left alone", func(t *testing.T) {
		for _, err := range []error{
			nil, agent.ErrBusy, fmt.Errorf("x: %w", agent.ErrNotSent), context.Canceled, agent.ErrForbidden, agent.ErrTokenRequired,
			// a status alone, without the sentinel the pool joins to it, is not for this hook
			&dialect.HTTPStatusError{Code: 403}, fmt.Errorf("send: %w", &dialect.HTTPStatusError{Code: 401}),
		} {
			f := newFixture(t, v1alpha1.Identity{TokenSecretRef: tokenRef()})
			c := call
			c.Err = err
			if got := f.source().Rejected(t.Context(), user, c); got != nil {
				t.Errorf("Rejected(%v) = %v", err, got)
			}
			if f.reader.count() != 0 || len(f.store.entries()) != 0 || len(f.metric) != 0 {
				t.Errorf("Rejected(%v) re-read, audited or counted", err)
			}
		}
	})

	t.Run("a token that is now unreadable is reported, and still audited and counted", func(t *testing.T) {
		f := newFixture(t, v1alpha1.Identity{TokenSecretRef: tokenRef()})
		err := f.source().Rejected(t.Context(), user, call)
		if !errors.Is(err, agent.ErrTokenUnreadable) {
			t.Errorf("Rejected = %v, want agent.ErrTokenUnreadable", err)
		}
		if len(f.store.entries()) != 1 || len(f.metric) != 1 {
			t.Errorf("%d audit rows and %d counts, want 1 each", len(f.store.entries()), len(f.metric))
		}
	})

	t.Run("a failed audit write is returned, and the re-read and the count still happen", func(t *testing.T) {
		f := newFixture(t, v1alpha1.Identity{TokenSecretRef: tokenRef()})
		f.reader.vals = map[string]string{"sre/creds/token": "t"}
		errDisk := errors.New("disk full")
		f.store.err = errDisk
		if err := f.source().Rejected(t.Context(), user, call); !errors.Is(err, errDisk) {
			t.Errorf("Rejected = %v, want the audit error", err)
		}
		if f.reader.count() != 1 || len(f.metric) != 1 {
			t.Errorf("%d reads and %d counts, want 1 each", f.reader.count(), len(f.metric))
		}
	})

	t.Run("no token secret, nothing to re-read", func(t *testing.T) {
		f := newFixture(t, v1alpha1.Identity{UserID: "svc"})
		if err := f.source().Rejected(t.Context(), user, call); err != nil {
			t.Fatal(err)
		}
		if f.reader.count() != 0 || len(f.store.entries()) != 1 || len(f.metric) != 1 {
			t.Errorf("%d reads, %d rows, %d counts; want 0, 1, 1", f.reader.count(), len(f.store.entries()), len(f.metric))
		}
	})
}

func TestNewRefusesANilAuditor(t *testing.T) {
	f := newFixture(t, v1alpha1.Identity{UserID: "svc"})
	if s, err := New(f.binding, f.backend, f.reader, nil, Options{}); err == nil || s != nil {
		t.Errorf("New(nil Auditor) = %v, %v, want an error and no Source", s, err)
	}
}

// fakeToken stands in for a later client-credentials source.
type fakeToken struct {
	token     string
	refreshes int
}

func (f *fakeToken) Token(context.Context) (secret.Value, error) { return secret.New(f.token), nil }
func (f *fakeToken) Refresh(context.Context) error               { f.refreshes++; return nil }

// TestAnyTokenSourceFitsTheSeam: a Source given another TokenSource asks it for the token,
// requires the token although the Binding has no tokenSecretRef, and refreshes it on a
// rejection.
func TestAnyTokenSourceFitsTheSeam(t *testing.T) {
	f := newFixture(t, v1alpha1.Identity{UserID: "svc"})
	ft := &fakeToken{token: "minted"}
	s := mustNew(t, f.binding, f.backend, f.reader, newWriter(t, f.store, audit.Options{}), Options{Token: ft})
	got, err := s.Credentials(t.Context())
	if err != nil || got.Token.Reveal() != "minted" {
		t.Fatalf("Credentials = %s, %v", plain(got), err)
	}
	if !s.Target().TokenRequired {
		t.Error("TokenRequired is false with a TokenSource")
	}
	if err := s.Rejected(t.Context(), audit.Actor{}, audit.Call{Op: audit.OpSend, MessageID: "m", Err: errRejection}); err != nil || ft.refreshes != 1 {
		t.Errorf("Rejected = %v after %d refreshes, want nil after 1", err, ft.refreshes)
	}
	if f.reader.count() != 0 {
		t.Error("the static reader was used")
	}
}

// TestSecretValuesNeverPrinted plants a canary in the token and in a header, then formats
// and logs everything a caller can reach, on the success path, the failure paths and the
// rejection.
func TestSecretValuesNeverPrinted(t *testing.T) {
	f := newFixture(t, v1alpha1.Identity{UserID: "svc", TokenSecretRef: tokenRef()},
		v1alpha1.Header{Name: "X-Api-Key", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "gw"}})
	f.reader.vals = map[string]string{"sre/creds/token": canary, "platform/gw/token": canary}
	s := f.source()
	ctx := t.Context()

	var seen []string
	note := func(what string, v any) {
		seen = append(seen, fmt.Sprintf("%s: %v %+v %#v %s %q %x", what, v, v, v, v, v, v))
	}
	creds, err := s.Credentials(ctx)
	if err != nil || creds.Token.Reveal() != canary || header(creds, "X-Api-Key") != canary {
		t.Fatalf("Credentials = %v, %v", err, creds)
	}
	note("credentials", creds)
	note("source", s)
	note("source copy", *s)
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
	note("static token copy", *NewStaticToken(f.reader, "sre", *tokenRef()))

	// Failure paths, and the rejection with its audit write, log and metric.
	f.reader.setErr(fmt.Errorf("upstream: %w", secret.ErrEmptyValue))
	_, err = s.Credentials(ctx)
	note("unreadable token", err)
	f.reader.setErr(nil)
	f.reader.set("sre/creds/token", canary)
	delete(f.reader.vals, "platform/gw/token")
	_, err = s.Credentials(ctx)
	note("unreadable header", err)
	f.reader.set("sre/creds/token", canary+"\n")
	_, err = s.Credentials(ctx)
	note("token with a newline", err)
	f.reader.set("sre/creds/token", canary)
	err = s.Rejected(ctx, audit.Actor{User: "U1"}, audit.Call{Op: audit.OpSend, MessageID: "m", Err: errRejection})
	note("rejected", err)
	f.store.err = errors.New("disk full")
	err = s.Rejected(ctx, audit.Actor{User: "U1"}, audit.Call{Op: audit.OpSend, MessageID: "m", Err: errRejection})
	note("rejected, audit failing", err)
	seen = append(seen, f.logs.String(), fmt.Sprintf("%+v", f.store.entries()))

	for _, out := range seen {
		if strings.Contains(out, canary) {
			t.Errorf("the canary leaked: %s", out)
		}
	}
	if !strings.Contains(strings.Join(seen, "\n"), "audit write failed") {
		t.Error("the failing audit write was not logged; the log was not exercised")
	}
}

// header returns the first value of a static header. Tests may Reveal; the code under test
// may not.
func header(c dialect.Credentials, name string) string {
	if v := c.Headers[name]; len(v) > 0 {
		return v[0].Reveal()
	}
	return ""
}

// plain renders c with its secrets revealed, to compare two Credentials in a test.
func plain(c dialect.Credentials) string {
	h := map[string][]string{}
	for name, vals := range c.Headers {
		for _, v := range vals {
			h[name] = append(h[name], v.Reveal())
		}
	}
	return fmt.Sprintf("token %q userID %q headers %q", c.Token.Reveal(), c.UserID, h)
}

func TestIdentityNamesTheTokenSecretOrTheUserID(t *testing.T) {
	if got := newFixture(t, v1alpha1.Identity{TokenSecretRef: tokenRef(), UserID: "svc"}).source().Identity(); got != "secret:sre/creds" {
		t.Errorf("Identity with a token = %q, want the Secret", got)
	}
	if got := newFixture(t, v1alpha1.Identity{UserID: "svc"}).source().Identity(); got != "svc" {
		t.Errorf("Identity with a userId only = %q, want it", got)
	}
}
