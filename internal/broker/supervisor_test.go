package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// newTestMetrics is the broker's metrics over a registry of its own.
func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	m, err := NewMetrics(func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var errRejected = errors.New("fake: token rejected")

// fakeConn is the platform of a connection on a double: Run holds its connection until its context ends, or until it is made to fail
// the way a rejected credential does. Nothing else of the platform is called by the supervisor.
type fakeConn struct {
	chat.Platform
	token  string
	live   *atomic.Int32
	reject chan struct{} // closed: Run returns the rejection
}

func (f *fakeConn) Run(ctx context.Context, _ chat.Sink) error {
	f.live.Add(1)
	defer f.live.Add(-1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.reject:
		return errRejected
	}
}

func (f *fakeConn) Classify(_ chat.Op, err error) chat.Verdict {
	if errors.Is(err, errRejected) {
		return chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}
	}
	return chat.Verdict{}
}

// supWorld is a supervisor over a runtime host whose platforms are doubles, and what it did.
type supWorld struct {
	t     *testing.T
	sup   *supervisor
	host  *runtimeHost
	table *runtimeTable
	live  atomic.Int32

	mu      sync.Mutex
	opened  []*fakeConn            // every platform built, in order
	closed  []string               // the connections whose transport was closed, in order
	gone    []string               // the connections the supervisor told were gone
	ids     map[string]appIdentity // by bot token
	idErr   map[string]error       // by bot token
	idented []string               // the tokens auth.test was asked for, in order
}

const ownName = "platform/slack"

func newSupWorld(t *testing.T) *supWorld {
	t.Helper()
	w := &supWorld{t: t, table: newRuntimeTable(), ids: map[string]appIdentity{}, idErr: map[string]error{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w.host = &runtimeHost{
		cfg: RunConfig{transportClosed: func(conn string) { w.mu.Lock(); w.closed = append(w.closed, conn); w.mu.Unlock() }},
		log: log, metrics: newTestMetrics(t), table: w.table, parent: t.Context(),
		open: func(c Connection, _ func(bool)) (builtPlatform, error) {
			f := &fakeConn{token: c.Credentials["botToken"].Reveal(), live: &w.live, reject: make(chan struct{})}
			w.mu.Lock()
			w.opened = append(w.opened, f)
			w.mu.Unlock()
			return builtPlatform{Platform: f, closeIdle: func() {}}, nil
		},
	}
	own := Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("own-token"), "appToken": secret.New("own-app")}}
	w.ids["own-token"] = appIdentity{Team: "T1", User: "UOWN", Bot: "BOWN"}
	ownRT, err := w.host.build(own, appIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	w.host.run(ownRT, nil, nil)
	w.sup = &supervisor{
		host: w.host, log: log, every: time.Hour, own: own, refusals: &refusals{}, done: make(chan struct{}),
		teams: map[string]*teamRuntime{}, said: map[string]string{},
		identify: func(_ context.Context, c Connection) (appIdentity, error) {
			tok := c.Credentials["botToken"].Reveal()
			w.mu.Lock()
			defer w.mu.Unlock()
			w.idented = append(w.idented, tok)
			return w.ids[tok], w.idErr[tok]
		},
		intake: func(v1alpha1.ObjectRef) (chat.Sink, error) { return nil, nil },
		gone:   func(conn string) { w.mu.Lock(); w.gone = append(w.gone, conn); w.mu.Unlock() },
	}
	t.Cleanup(func() {
		w.host.haltAll(context.Background())
		w.host.closeAll()
	})
	return w
}

// team is a served team ChatConnection whose bot token is token, made at age (smaller is older).
func team(name string, gen int64, token string, age int) TeamConnection {
	ns, n, _ := strings.Cut(name, "/")
	return TeamConnection{
		Connection: Connection{Ref: v1alpha1.ObjectRef{Namespace: ns, Name: n}, Platform: v1alpha1.PlatformSlack,
			Credentials: map[string]secret.Value{"botToken": secret.New(token), "appToken": secret.New("app-" + token)}},
		UID: "uid-" + name, Generation: gen, Created: time.Unix(int64(1000+age), 0),
	}
}

// unready is team without credentials: its Secret cannot be read yet.
func unready(name string, gen int64, age int) TeamConnection {
	t := team(name, gen, "", age)
	t.Credentials, t.Err = nil, errors.New("Secret not synced")
	return t
}

func (w *supWorld) reconcile(desired ...TeamConnection) { w.sup.reconcile(w.t.Context(), desired) }

func (w *supWorld) running() []string {
	var out []string
	for _, rt := range w.table.all() {
		out = append(out, rt.name)
	}
	slices.Sort(out)
	return out
}

func (w *supWorld) tokens() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, f := range w.opened {
		out = append(out, f.token)
	}
	return out
}

func (w *supWorld) eventually(what string, ok func() bool) {
	w.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// A team connection is started when the Snapshot serves it, left alone while it is as it was, restarted when its generation or the content of
// its Secret changes (the old platform is stopped and its transport closed before the new one runs), and stopped when the Snapshot no longer
// serves it. The own connection is none of its business.
func TestSupervisorAddsRestartsAndRemovesATeamConnection(t *testing.T) {
	before := goleak.IgnoreCurrent()
	w := newSupWorld(t)
	w.ids["tok-a1"], w.ids["tok-a2"], w.ids["tok-a3"] = appIdentity{Team: "T1", User: "UA", Bot: "BA"}, appIdentity{Team: "T1", User: "UA", Bot: "BA"}, appIdentity{Team: "T1", User: "UA", Bot: "BA"}

	w.reconcile(team("team-a/slack", 1, "tok-a1", 1))
	w.eventually("the connection to run", func() bool { return w.live.Load() == 2 })
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Fatalf("running = %v; want the own connection and team-a", got)
	}

	w.reconcile(team("team-a/slack", 1, "tok-a1", 1))
	if got := w.tokens(); !slices.Equal(got, []string{"own-token", "tok-a1"}) {
		t.Errorf("platforms built = %v after a pass that found nothing new; want no more", got)
	}

	w.reconcile(team("team-a/slack", 2, "tok-a1", 1)) // the spec changed
	if got := w.tokens(); !slices.Equal(got, []string{"own-token", "tok-a1", "tok-a1"}) {
		t.Errorf("platforms built = %v after a new generation; want a restart", got)
	}
	w.reconcile(team("team-a/slack", 2, "tok-a2", 1)) // the Secret's content changed
	if got := w.tokens(); !slices.Equal(got, []string{"own-token", "tok-a1", "tok-a1", "tok-a2"}) {
		t.Errorf("platforms built = %v after a new token; want a restart with it", got)
	}
	w.eventually("only the restarted run to be live", func() bool { return w.live.Load() == 2 })
	w.mu.Lock()
	closed := slices.Clone(w.closed)
	w.mu.Unlock()
	if want := []string{"team-a/slack", "team-a/slack"}; !slices.Equal(closed, want) {
		t.Errorf("transports closed = %v; want those of the two runs that were replaced", closed)
	}

	w.reconcile() // the Snapshot serves it no more
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Errorf("running = %v after the connection left the Snapshot; want the own alone", got)
	}
	if _, err := w.table.Platform("team-a/slack"); !errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("Platform(team-a) = %v; want ErrConnectionDown", err)
	}
	w.mu.Lock()
	gone := slices.Clone(w.gone)
	w.mu.Unlock()
	if !slices.Equal(gone, []string{"team-a/slack", "team-a/slack", "team-a/slack"}) {
		t.Errorf("gone = %v; want it told each time a runtime was taken away", gone)
	}
	w.eventually("the removed connection's run to be gone", func() bool { return w.live.Load() == 1 })

	w.host.haltAll(t.Context())
	w.host.closeAll()
	goleak.VerifyNone(t, before)
}

// A team connection whose Secret cannot be read does not start and does not fail the pass; it starts on the pass that can read it. One
// that runs keeps running when its Secret cannot be read for a moment.
func TestSupervisorStartsAConnectionOnceItsSecretCanBeRead(t *testing.T) {
	w := newSupWorld(t)
	w.ids["tok-a"], w.ids["tok-b"] = appIdentity{Team: "T1", User: "UA", Bot: "BA"}, appIdentity{Team: "T1", User: "UB", Bot: "BB"}

	w.reconcile(unready("team-a/slack", 1, 1), team("team-b/slack", 1, "tok-b", 2))
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Fatalf("running = %v; want team-a left to start later and team-b started", got)
	}

	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack", "team-b/slack"}) {
		t.Errorf("running = %v once the Secret is there; want team-a to run", got)
	}

	w.reconcile(team("team-a/slack", 1, "tok-a", 1), unready("team-b/slack", 1, 2)) // the cache of team-b's Secret is relisting
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack", "team-b/slack"}) {
		t.Errorf("running = %v with team-b's Secret unreadable; want it to keep running on the tokens it started with", got)
	}
}

// A team connection whose platform rejects its credentials stops itself and leaves the table: the connection is down, nothing else is
// touched, and it runs again when its Secret changes.
func TestSupervisorLeavesTheOtherConnectionsWhenATeamConnectionIsRejected(t *testing.T) {
	w := newSupWorld(t)
	w.ids["tok-a"], w.ids["tok-b"], w.ids["tok-a2"] = appIdentity{Team: "T1", User: "UA", Bot: "BA"}, appIdentity{Team: "T1", User: "UB", Bot: "BB"}, appIdentity{Team: "T1", User: "UA", Bot: "BA"}
	a, b := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2)
	w.reconcile(a, b)
	w.eventually("three runs", func() bool { return w.live.Load() == 3 })

	w.mu.Lock()
	rejected := w.opened[1]
	w.mu.Unlock()
	close(rejected.reject)
	w.eventually("team-a to leave the table", func() bool { return !slices.Contains(w.running(), "team-a/slack") })
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Errorf("running = %v after team-a was rejected; want the others", got)
	}
	if _, err := w.table.Platform("team-a/slack"); !errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("Platform(team-a) = %v; want ErrConnectionDown", err)
	}

	w.reconcile(a, b) // unchanged: it is not started again for nothing
	if got := w.tokens(); !slices.Equal(got, []string{"own-token", "tok-a", "tok-b"}) {
		t.Errorf("platforms built = %v; want no restart of a rejected connection whose Secret is as it was", got)
	}
	w.reconcile(team("team-a/slack", 1, "tok-a2", 1), b)
	w.eventually("team-a to run again", func() bool { return slices.Contains(w.running(), "team-a/slack") })
}

// Two connections of one Slack app are not both served: the older is, the own connection first. The younger says so to its Bindings (the
// refusals), and is served once the older goes. A younger one that runs is stopped when an older of its app turns up.
func TestSupervisorServesTheOlderConnectionOfASlackApp(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-old"], w.ids["tok-young"], w.ids["tok-own-twin"] = app, app, appIdentity{Team: "T1", User: "UOWN", Bot: "BOWN"}
	old, young := team("team-a/slack", 1, "tok-old", 1), team("team-b/slack", 1, "tok-young", 2)

	w.reconcile(old, young) // desired is oldest first, as teamConnections gives it
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Fatalf("running = %v; want the older connection of the app and not the younger", got)
	}
	if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Errorf("refused = %v; want the younger connection", got)
	}

	w.reconcile(young) // the older is gone
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Errorf("running = %v once the older is gone; want the younger to be served", got)
	}
	if got := w.sup.refusals.names(); len(got) != 0 {
		t.Errorf("refused = %v; want none", got)
	}

	w.reconcile(old, young) // an older connection of the app that the younger holds turns up
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Errorf("running = %v when the older turned up; want it served and the younger stopped", got)
	}

	// A connection of the app the own connection uses is never served, whatever its age.
	w.ids["tok-twin"] = appIdentity{Team: "T1", User: "UOWN", Bot: "BOWN"}
	w.reconcile(team("team-c/slack", 1, "tok-twin", 0))
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Errorf("running = %v with a twin of the own app; want the own alone", got)
	}
	if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-c/slack"}) {
		t.Errorf("refused = %v; want the twin", got)
	}
}

// A connection whose bot cannot be asked about waits (nothing starts on a guess), and the others go on. The own connection's identity is
// asked first, so that no team connection starts before it is known.
func TestSupervisorWaitsForAnIdentityItCannotGet(t *testing.T) {
	w := newSupWorld(t)
	w.ids["tok-b"] = appIdentity{Team: "T1", User: "UB", Bot: "BB"}
	w.idErr["tok-a"] = errors.New("auth.test: timed out")

	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Fatalf("running = %v; want team-a to wait for its identity", got)
	}
	delete(w.idErr, "tok-a")
	w.ids["tok-a"] = appIdentity{Team: "T1", User: "UA", Bot: "BA"}
	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack", "team-b/slack"}) {
		t.Errorf("running = %v once the identity is known; want team-a to run", got)
	}

	w2 := newSupWorld(t)
	w2.idErr["own-token"] = errors.New("auth.test: timed out")
	w2.reconcile(team("team-a/slack", 1, "tok-a", 1))
	if got := w2.running(); !slices.Equal(got, []string{ownName}) {
		t.Errorf("running = %v with the own identity unknown; want no team connection", got)
	}
}

// The loop follows the Snapshot: a team connection whose Secret is not there when it begins (a takeover before the Secret informer synced
// it) starts on a later look, and a connection taken out of the Snapshot stops, with no goroutine of its run left.
func TestSupervisorLoopFollowsTheSnapshot(t *testing.T) {
	before := goleak.IgnoreCurrent()
	w := newSupWorld(t)
	w.ids["tok-a"] = appIdentity{Team: "T1", User: "UA", Bot: "BA"}
	r := &reader{vals: map[string]string{}}
	var snap atomic.Pointer[config.Snapshot]
	snap.Store(slackConnections(t, "platform/slack", "team-a/slack"))
	w.sup.snapshot, w.sup.secrets, w.sup.every = snap.Load, r, time.Millisecond
	ctx, stop := context.WithCancel(t.Context())

	go w.sup.run(ctx, teamConnections(ctx, snap.Load(), w.sup.own.Ref, r))
	time.Sleep(20 * time.Millisecond)
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Fatalf("running = %v with team-a's Secret missing; want the own connection alone", got)
	}
	r.set("team-a/creds-slack/botToken", "tok-a")
	r.set("team-a/creds-slack/appToken", "app-a")
	w.eventually("team-a to start once its Secret is there", func() bool { return slices.Contains(w.running(), "team-a/slack") })

	snap.Store(slackConnections(t, "platform/slack"))
	w.eventually("team-a to stop", func() bool { return !slices.Contains(w.running(), "team-a/slack") && w.live.Load() == 1 })

	stop()
	<-w.sup.done
	w.host.haltAll(t.Context())
	w.host.closeAll()
	goleak.VerifyNone(t, before)
}
