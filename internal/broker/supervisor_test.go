package broker

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
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
	// onConn is the adapter's OnConnection, which Run calls when its connection is up.
	onConn func(bool)

	mu    sync.Mutex
	calls map[string]int
	errs  map[string]error // the error each call of the platform answers with, by name: "Post", "Stream", "Update", "Ephemeral", "IsMember"
}

// failWith makes the call op of the platform answer with err from now on.
func (f *fakeConn) failWith(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[op] = err
}

func (f *fakeConn) answer(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[op]++
	return f.errs[op]
}

// called is how many times the call op reached the platform.
func (f *fakeConn) called(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *fakeConn) Post(context.Context, chat.Target, chat.Outgoing, string) (chat.Ref, error) {
	return chat.Ref{}, f.answer("Post")
}
func (f *fakeConn) Update(context.Context, chat.Ref, chat.Outgoing) error { return f.answer("Update") }
func (f *fakeConn) Stream(context.Context, chat.Target, string, string) (chat.Stream, error) {
	return nil, f.answer("Stream")
}
func (f *fakeConn) StreamAt(chat.Ref) chat.Stream { return fakeStream{f} }
func (f *fakeConn) Ephemeral(context.Context, chat.Target, string, chat.Message) error {
	return f.answer("Ephemeral")
}
func (f *fakeConn) IsMember(context.Context, string, string) (bool, error) {
	return false, f.answer("IsMember")
}

func (f *fakeConn) Run(ctx context.Context, _ chat.Sink) error {
	if f.onConn != nil { // before the count, so that a test that waits for the count sees the connection up
		f.onConn(true)
	}
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
	opened  []*fakeConn // every platform built, in order
	closed  []string    // the connections whose transport was closed, in order
	gone    []string    // the connections the supervisor told were gone
	removed []string    // the connections the supervisor told were removed
	recov   []string    // the connections the supervisor asked to have their turns taken up, in order
	recErr  error       // what that answers
	logs    syncBuffer
	ids     map[string]appIdentity // by bot token
	idErr   map[string]error       // by bot token
	openErr map[string]error       // by bot token: building the platform fails
	idented []string               // the tokens auth.test was asked for, in order
}

const ownName = "platform/slack"

func newSupWorld(t *testing.T) *supWorld {
	t.Helper()
	w := &supWorld{t: t, table: newRuntimeTable(), ids: map[string]appIdentity{}, idErr: map[string]error{}, openErr: map[string]error{}}
	log := slog.New(slog.NewTextHandler(&w.logs, nil))
	w.host = &runtimeHost{
		cfg: RunConfig{transportClosed: func(conn string) { w.mu.Lock(); w.closed = append(w.closed, conn); w.mu.Unlock() }},
		log: log, metrics: newTestMetrics(t), table: w.table, parent: t.Context(),
		open: func(c Connection, onConn func(bool)) (builtPlatform, error) {
			f := &fakeConn{token: c.Credentials["botToken"].Reveal(), live: &w.live, reject: make(chan struct{}), onConn: onConn, errs: map[string]error{}, calls: map[string]int{}}
			w.mu.Lock()
			defer w.mu.Unlock()
			if err := w.openErr[f.token]; err != nil {
				return builtPlatform{}, err
			}
			w.opened = append(w.opened, f)
			return builtPlatform{Platform: f, closeIdle: func() {}}, nil
		},
	}
	own := Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("own-token"), "appToken": secret.New("own-app")}}
	w.ids["own-token"] = appIdentity{App: "AOWN", Team: "T1", User: "UOWN", Bot: "BOWN"}
	ownRT, err := w.host.build(own)
	if err != nil {
		t.Fatal(err)
	}
	w.host.run(ownRT, nil, nil)
	w.sup = &supervisor{
		host: w.host, log: log, every: time.Hour, own: own, refusals: &refusals{}, done: make(chan struct{}),
		teams: map[string]*teamRuntime{}, seen: map[string]bool{}, lives: map[string]*connLife{}, said: map[string]string{},
		identify: func(_ context.Context, c Connection) (appIdentity, error) {
			tok := c.Credentials["botToken"].Reveal()
			w.mu.Lock()
			defer w.mu.Unlock()
			w.idented = append(w.idented, tok)
			return w.ids[tok], w.idErr[tok]
		},
		conns: new(atomic.Pointer[[]Connection]), changes: new(atomic.Uint64),
		intake: func(v1alpha1.ObjectRef) (chat.Sink, error) { return nil, nil },
		gone:   func(conn string) { w.mu.Lock(); w.gone = append(w.gone, conn); w.mu.Unlock() },
		removed: func(ctx context.Context, conn string) {
			w.mu.Lock()
			w.removed = append(w.removed, conn)
			w.mu.Unlock()
			w.host.metrics.ConnectionRemoved(ctx, conn)
		},
		recover: func(_ context.Context, conn string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.recov = append(w.recov, conn)
			return w.recErr
		},
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
	w.ids["tok-a1"], w.ids["tok-a2"], w.ids["tok-a3"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}, appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}, appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}

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
	w.ids["tok-a"], w.ids["tok-b"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}, appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}

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
	w.ids["tok-a"], w.ids["tok-b"], w.ids["tok-a2"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}, appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}, appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
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
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-old"], w.ids["tok-young"], w.ids["tok-own-twin"] = app, app, appIdentity{App: "AOWN", Team: "T1", User: "UOWN", Bot: "BOWN"}
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
	w.ids["tok-twin"] = appIdentity{App: "AOWN", Team: "T1", User: "UOWN", Bot: "BOWN"}
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
	w.ids["tok-b"] = appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}
	w.idErr["tok-a"] = errors.New("auth.test: timed out")

	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Fatalf("running = %v; want team-a to wait for its identity", got)
	}
	delete(w.idErr, "tok-a")
	w.ids["tok-a"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
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
	w.ids["tok-a"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	r := &reader{vals: map[string]string{}}
	var snap atomic.Pointer[config.Snapshot]
	snap.Store(slackConnections(t, "platform/slack", "team-a/slack"))
	w.sup.snapshot, w.sup.secrets, w.sup.every = snap.Load, r, time.Millisecond
	ctx, stop := context.WithCancel(t.Context())

	go w.sup.run(ctx, teamConnections(ctx, snap.Load(), w.sup.own.Ref, r))
	w.eventually("the supervisor to have looked more than once", func() bool { return w.sup.passes.Load() >= 3 })
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

// One app installed in two workspaces is one app: its two ChatConnections have different workspaces, bot users and bot IDs and one app ID, and
// the older is served and the other is not run.
func TestSupervisorServesAnAppInstalledInTwoWorkspacesOnce(t *testing.T) {
	w := newSupWorld(t)
	w.ids["tok-w1"] = appIdentity{App: "AAPP", Team: "TW1", User: "UW1", Bot: "BW1"}
	w.ids["tok-w2"] = appIdentity{App: "AAPP", Team: "TW2", User: "UW2", Bot: "BW2"}

	w.reconcile(team("team-a/slack", 1, "tok-w1", 1), team("team-b/slack", 1, "tok-w2", 2))

	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Errorf("running = %v; want the older installation of the app and not the other", got)
	}
	if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Errorf("refused = %v; want the second installation", got)
	}
}

// The app is asked of Slack with auth.test and then bots.info, and a connection is not started on less: a bots.info that fails delays the start
// to a look at which it answers, and is never made up from the workspace and user.
func TestSupervisorDoesNotStartAConnectionWhoseBotsInfoFails(t *testing.T) {
	w := newSupWorld(t)
	f := newFakeSlackWeb(t)
	f.bots["tok-a"] = struct{ team, user, bot, app string }{"TW1", "UA", "BA", "AA"}
	w.sup.identify = func(ctx context.Context, c Connection) (appIdentity, error) {
		if c.Credentials["botToken"].Reveal() == "own-token" {
			return w.ids["own-token"], nil
		}
		return slackIdentity(ctx, c, f.url())
	}
	f.fail["bots.info"] = "ratelimited"

	w.reconcile(team("team-a/slack", 1, "tok-a", 1))
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Fatalf("running = %v with bots.info failing; want the connection not to start", got)
	}

	delete(f.fail, "bots.info")
	w.reconcile(team("team-a/slack", 1, "tok-a", 1))
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Errorf("running = %v once bots.info answers; want the connection to start", got)
	}
}

// A token that Slack refuses is not asked about again until the ChatConnection or its Secret changes: it would be refused every five seconds
// for ever. A new token is asked.
func TestSupervisorDoesNotAskAgainAboutRefusedCredentials(t *testing.T) {
	w := newSupWorld(t)
	w.idErr["tok-a"] = fmt.Errorf("auth.test: Slack answered %q: %w", "invalid_auth", errCredentialsRejected)
	w.ids["tok-a2"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	for range 4 {
		w.reconcile(team("team-a/slack", 1, "tok-a", 1))
	}
	w.mu.Lock()
	n := len(slices.DeleteFunc(slices.Clone(w.idented), func(s string) bool { return s != "tok-a" }))
	w.mu.Unlock()
	if n != 1 {
		t.Errorf("the refused token was asked about %d times in four looks; want once", n)
	}

	w.reconcile(team("team-a/slack", 1, "tok-a2", 1)) // its Secret was fixed
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Errorf("running = %v with a new token; want it asked and started", got)
	}
}

// A connection whose start failed holds no Slack app: a younger connection of the same app is served meanwhile, instead of nothing serving it.
func TestSupervisorLetsAYoungerConnectionServeAnAppWhoseOlderCouldNotStart(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-old"], w.ids["tok-young"] = app, app
	w.openErr["tok-old"] = errors.New("no adapter")
	old, young := team("team-a/slack", 1, "tok-old", 1), team("team-b/slack", 1, "tok-young", 2)

	w.reconcile(old, young) // the older holds the app for this pass, and fails to start
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Fatalf("running = %v after the first look; want the older's start to have failed and the younger refused for now", got)
	}
	w.reconcile(old, young)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Errorf("running = %v at the next look; want the younger to serve the app the older cannot", got)
	}
	if got := w.sup.refusals.names(); len(got) != 0 {
		t.Errorf("refused = %v; want none while the younger serves", got)
	}
}

// Once the shutdown has halted the runtimes the table takes no more: a runtime the supervisor builds after that is refused, so none runs on
// past the step that stops them.
func TestTheTableTakesNoRuntimeOnceTheShutdownHasHaltedThem(t *testing.T) {
	w := newSupWorld(t)
	w.host.haltAll(t.Context())
	own := w.sup.own
	own.Ref.Name = "late"
	if rt, err := w.host.build(own); !errors.Is(err, errStopping) || rt != nil {
		t.Errorf("build after haltAll = %v, %v; want errStopping and no runtime", rt, err)
	}
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Errorf("table = %v; want the runtime that was there and no more", got)
	}
}

// The supervisor publishes the connections the verification probes the Bindings of: the own connection with its credentials, and each team
// connection the Snapshot serves that was not refused, with the credentials of its runtime if it runs and none if it does not (a connection that is not
// running has no baseline to tell a changed Secret against). A refused connection is left out, so its Bindings are not probed. The list is replaced
// whole at each pass, and the one a reader holds is never changed.
func TestSupervisorPublishesTheConnectionsToVerify(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a"], w.ids["tok-twin"], w.ids["tok-b"] = app, app, appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}
	published := func() map[string]string { // the connections, with the bot token each has
		out := map[string]string{}
		for _, c := range *w.sup.conns.Load() {
			out[c.Ref.Namespace+"/"+c.Ref.Name] = c.Credentials["botToken"].Reveal()
		}
		return out
	}

	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-twin", 2), unready("team-c/slack", 1, 3), team("team-d/slack", 1, "tok-b", 4))
	first := *w.sup.conns.Load()
	got := published()
	want := map[string]string{ownName: "own-token", "team-a/slack": "tok-a", "team-c/slack": "", "team-d/slack": "tok-b"}
	if len(got) != len(want) {
		t.Fatalf("published = %v; want %v: the refused connection left out, the unready one in with no credentials", got, want)
	}
	for conn, tok := range want {
		if g, ok := got[conn]; !ok || g != tok {
			t.Errorf("published = %v; want %v", got, want)
			break
		}
	}

	w.reconcile(team("team-a/slack", 2, "tok-a2", 1)) // changed, and the others gone
	if got := published(); len(got) != 2 || got["team-a/slack"] != "tok-a2" {
		t.Errorf("published = %v; want the own connection and team-a on its new token", got)
	}
	if first[1].Credentials["botToken"].Reveal() != "tok-a" || len(first) != 4 {
		t.Errorf("the list a reader held changed: %+v", first)
	}

	w.reconcile()
	if got := published(); len(got) != 1 || got[ownName] != "own-token" {
		t.Errorf("published = %v with no team connection; want the own alone", got)
	}
}

// The published list counts a change and only a change: a pass that finds the connections and credentials as they were adds nothing, and a start, a
// restart on a new token and a stop each add to it, which is what calls for a pass of the verification.
func TestSupervisorCountsAChangeOfThePublishedConnections(t *testing.T) {
	w := newSupWorld(t)
	w.ids["tok-a"], w.ids["tok-a2"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}, appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	steps := []struct {
		desired []TeamConnection
		counts  bool
		what    string
	}{
		{nil, false, "no team connection, the list the leader stored"},
		{[]TeamConnection{team("team-a/slack", 1, "tok-a", 1)}, true, "a connection started"},
		{[]TeamConnection{team("team-a/slack", 1, "tok-a", 1)}, false, "a pass that finds it as it was"},
		{[]TeamConnection{team("team-a/slack", 2, "tok-a2", 1)}, true, "a restart on a new token"},
		{nil, true, "a connection stopped"},
	}
	w.sup.conns.Store(&[]Connection{w.sup.own})
	for _, st := range steps {
		before := w.sup.changes.Load()
		w.reconcile(st.desired...)
		if changed := w.sup.changes.Load() != before; changed != st.counts {
			t.Errorf("after %s: counted a change = %t; want %t", st.what, changed, st.counts)
		}
	}
}

// fakeStream is the stream of a fakeConn: its Append answers as the platform's "Append".
type fakeStream struct{ f *fakeConn }

func (s fakeStream) Ref() chat.Ref                         { return chat.Ref{} }
func (s fakeStream) Append(context.Context, string) error  { return s.f.answer("Append") }
func (s fakeStream) Step(context.Context, chat.Step) error { return s.f.answer("Step") }
func (s fakeStream) Stop(context.Context, string, chat.Message, string) error {
	return s.f.answer("Stop")
}

// seriesWorld is a supWorld that records under an observer, for the tests of what the supervisor does to a connection's series.
type seriesWorld struct {
	*supWorld
	h *telemetrytest.Harness
}

func newSeriesWorld(t *testing.T) *seriesWorld {
	return &seriesWorld{supWorld: newSupWorld(t), h: telemetrytest.New(t)}
}

// record counts a chat call of each connection, as the adapter of the connection does.
func (w *seriesWorld) record(conns ...string) {
	for _, c := range conns {
		telemetry.Count(w.h.Ctx, "agentkourier_chat_api_calls_total", "Post", "success", c)
	}
}

// has says whether the connection has its series of chat calls.
func (w *seriesWorld) has(conn string) bool {
	return w.h.Counter(w.t, "agentkourier_chat_api_calls_total", map[string]string{"operation": "Post", "outcome": "success", "connection": conn}) > 0
}

func (w *seriesWorld) reconcile(desired ...TeamConnection) { w.sup.reconcile(w.h.Ctx, desired) }

// The series of a connection (telemetry.ConnectionLabel) are deleted when the Snapshot no longer serves the connection, and are kept when the
// connection only restarts, so that its counters go on. Those of the other connections stay.
func TestSupervisorDeletesTheSeriesOfAConnectionThatLeavesTheSnapshot(t *testing.T) {
	w := newSeriesWorld(t)
	w.ids["tok-a"], w.ids["tok-a2"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}, appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	w.ids["tok-b"] = appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}
	a, b := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2)

	w.reconcile(a, b)
	w.eventually("both connections to run", func() bool { return w.live.Load() == 3 })
	w.record(ownName, "team-a/slack", "team-b/slack")

	w.reconcile(team("team-a/slack", 2, "tok-a2", 1), b) // team-a restarts on a new generation and Secret
	if !w.has("team-a/slack") || !w.has("team-b/slack") || !w.has(ownName) {
		t.Fatal("a restart of team-a deleted a series; want every series kept")
	}

	w.reconcile(team("team-a/slack", 2, "tok-a2", 1)) // team-b left the Snapshot
	if w.has("team-b/slack") {
		t.Error("the series of team-b are still there after it left the Snapshot")
	}
	if !w.has("team-a/slack") || !w.has(ownName) {
		t.Error("the series of team-a or of the own connection went with team-b's")
	}
}

// A connection that is stopped because an older connection of its Slack app turned up is not served either, so its series go too.
func TestSupervisorDeletesTheSeriesOfAConnectionItStopsForAnOlderOfItsApp(t *testing.T) {
	w := newSeriesWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-old"], w.ids["tok-young"] = app, app
	old, young := team("team-a/slack", 1, "tok-old", 1), team("team-b/slack", 1, "tok-young", 2)

	w.reconcile(young)
	w.eventually("the younger connection to run", func() bool { return w.live.Load() == 2 })
	w.record("team-b/slack")
	w.reconcile(old, young) // the older connection of the app turns up: the younger is stopped and refused
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Fatalf("running = %v; want the older connection served and the younger stopped", got)
	}
	if w.has("team-b/slack") {
		t.Error("the series of team-b are still there after it was stopped for the older connection of its app")
	}
}

// A connection that restarts into a refusal is not served either: its Secret was rotated to the tokens of the Slack app that an older connection
// uses, so its new entry never ran, and the series of the run it replaced go.
func TestSupervisorDeletesTheSeriesOfAConnectionThatRestartsIntoARefusal(t *testing.T) {
	w := newSeriesWorld(t)
	w.ids["tok-a"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	w.ids["tok-b"] = appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}
	w.ids["tok-b2"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	a, b := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2)

	w.reconcile(a, b)
	w.eventually("both connections to run", func() bool { return w.live.Load() == 3 })
	w.record(ownName, "team-a/slack", "team-b/slack")

	w.reconcile(a, team("team-b/slack", 2, "tok-b2", 2)) // team-b's Secret now holds the tokens of team-a's app
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Fatalf("running = %v; want team-b stopped as a second connection of team-a's app", got)
	}
	if w.has("team-b/slack") {
		t.Error("the series of team-b are still there after it restarted into a refusal")
	}
	if !w.has("team-a/slack") || !w.has(ownName) {
		t.Error("the series of team-a or of the own connection went with team-b's")
	}
}

// A connection that restarts keeps the Slack app it held, and a refusal it was under, while the identity of its new tokens is being asked: a
// younger connection of the app is not served for the one look that Slack did not answer, to be refused again at the next.
func TestSupervisorKeepsTheAppOfAHolderWhoseIdentityIsBeingAsked(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a1"], w.ids["tok-b"] = app, app
	a1, b := team("team-a/slack", 1, "tok-a1", 1), team("team-b/slack", 1, "tok-b", 2)
	w.reconcile(a1, b)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Fatalf("running = %v; want team-a to serve the app and team-b refused", got)
	}

	w.idErr["tok-a2"] = mayPass("auth.test: timed out") // team-a's Secret is rotated to a token of the same app, and Slack does not answer
	a2 := team("team-a/slack", 1, "tok-a2", 1)
	for range 2 {
		w.reconcile(a2, b)
		if got := w.running(); !slices.Equal(got, []string{ownName}) {
			t.Fatalf("running = %v while team-a's identity is unknown; want team-a not started and team-b not served in its place", got)
		}
		if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
			t.Fatalf("refused = %v while team-a's identity is unknown; want team-b to stay refused", got)
		}
	}

	delete(w.idErr, "tok-a2")
	w.ids["tok-a2"] = app
	w.reconcile(a2, b)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Errorf("running = %v once the identity is known; want team-a to serve the app again", got)
	}
	if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Errorf("refused = %v once the identity is known; want team-b refused throughout", got)
	}
	if got := w.tokens(); slices.Contains(got, "tok-b") {
		t.Errorf("platforms built = %v; want none for team-b, which was never served", got)
	}
	w.mu.Lock()
	removed := slices.Clone(w.removed)
	w.mu.Unlock()
	if want := []string{"team-b/slack"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v; want team-b told once, when it was first refused", removed)
	}
}

// What a restart does not know of the new tokens does not decide the app: when the new tokens turn out to be those of another app, the app the
// connection held is free and the younger connection of it is served.
func TestSupervisorFreesTheAppOfAHolderWhoseNewTokenIsAnotherApp(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a1"], w.ids["tok-b"] = app, app
	a1, b := team("team-a/slack", 1, "tok-a1", 1), team("team-b/slack", 1, "tok-b", 2)
	w.reconcile(a1, b)

	w.idErr["tok-a2"] = mayPass("auth.test: timed out")
	a2 := team("team-a/slack", 1, "tok-a2", 1)
	w.reconcile(a2, b)
	if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Fatalf("refused = %v while team-a's identity is unknown; want team-b to stay refused", got)
	}

	delete(w.idErr, "tok-a2")
	w.ids["tok-a2"] = appIdentity{App: "AOTHER", Team: "T1", User: "UOTHER", Bot: "BOTHER"}
	w.reconcile(a2, b)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack", "team-b/slack"}) {
		t.Errorf("running = %v; want team-a on its other app and team-b serving the app team-a let go", got)
	}
	if got := w.sup.refusals.names(); len(got) != 0 {
		t.Errorf("refused = %v; want none", got)
	}
}

// A refused connection whose own tokens change stays refused while Slack does not say whose bot the new ones are, and is served when they are
// another app's.
func TestSupervisorKeepsTheRefusalOfAConnectionWhoseIdentityIsBeingAsked(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a"], w.ids["tok-b1"] = app, app
	a, b1 := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b1", 2)
	w.reconcile(a, b1)

	w.idErr["tok-b2"] = mayPass("bots.info: timed out")
	b2 := team("team-b/slack", 1, "tok-b2", 2)
	for range 2 {
		w.reconcile(a, b2)
		if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
			t.Fatalf("refused = %v while team-b's identity is unknown; want its refusal kept, and not lifted for the look", got)
		}
	}
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Errorf("running = %v; want team-a alone", got)
	}

	delete(w.idErr, "tok-b2")
	w.ids["tok-b2"] = appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}
	w.reconcile(a, b2)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack", "team-b/slack"}) {
		t.Errorf("running = %v once team-b's tokens are known to be another app's; want it served", got)
	}
	if got := w.sup.refusals.names(); len(got) != 0 {
		t.Errorf("refused = %v; want none", got)
	}
}

// A connection whose identity is being asked claims the app it held only if no older connection has it: it is not refused on a guess, and the
// older connection keeps the app.
func TestSupervisorDoesNotRefuseAConnectionOnTheAppItHeldBeforeItsTokensChanged(t *testing.T) {
	w := newSupWorld(t)
	w.ids["tok-a1"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	w.ids["tok-b1"] = appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}
	w.reconcile(team("team-a/slack", 1, "tok-a1", 1), team("team-b/slack", 1, "tok-b1", 2))

	w.ids["tok-a2"] = w.ids["tok-b1"] // the older connection's Secret now holds the tokens of the app team-b held
	w.idErr["tok-b2"] = mayPass("auth.test: timed out")
	a2, b2 := team("team-a/slack", 1, "tok-a2", 1), team("team-b/slack", 1, "tok-b2", 2)
	w.reconcile(a2, b2)
	if got := w.sup.refusals.names(); len(got) != 0 {
		t.Errorf("refused = %v; want team-b not refused for an app it may no longer use", got)
	}

	delete(w.idErr, "tok-b2")
	w.ids["tok-b2"] = appIdentity{App: "AC", Team: "T1", User: "UC", Bot: "BC"}
	w.reconcile(a2, b2)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack", "team-b/slack"}) {
		t.Errorf("running = %v; want both served once team-b is known to use another app", got)
	}
}

// The bot guard goes by the bots of the connections that are served, and not only by those that run now: a connection that restarts has no runtime
// until its new tokens are known, and the turns that hold its platform still post as its bot.
func TestTheBotGuardKeepsTheBotOfAConnectionThatRestarts(t *testing.T) {
	w := newSupWorld(t)
	bot := appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}
	w.ids["tok-b1"] = bot
	w.reconcile(team("team-b/slack", 1, "tok-b1", 1))
	if conn, ok := w.table.botOwner("BB"); !ok || conn != "team-b/slack" {
		t.Fatalf("botOwner(BB) = %q, %v; want team-b while it runs", conn, ok)
	}
	if conn, ok := w.table.botOwner("BOWN"); !ok || conn != ownName {
		t.Fatalf("botOwner(BOWN) = %q, %v; want the own connection", conn, ok)
	}

	w.idErr["tok-b2"] = mayPass("bots.info: timed out")
	w.reconcile(team("team-b/slack", 1, "tok-b2", 1))
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Fatalf("running = %v; want team-b stopped for the restart", got)
	}
	if conn, ok := w.table.botOwner("BB"); !ok || conn != "team-b/slack" {
		t.Errorf("botOwner(BB) = %q, %v while team-b restarts; want team-b", conn, ok)
	}
	if conn, ok := w.table.botUserOwner("UB"); !ok || conn != "team-b/slack" {
		t.Errorf("botUserOwner(UB) = %q, %v while team-b restarts; want team-b", conn, ok)
	}

	delete(w.idErr, "tok-b2") // a new identity replaces the old
	w.ids["tok-b2"] = appIdentity{App: "AB2", Team: "T1", User: "UB2", Bot: "BB2"}
	w.reconcile(team("team-b/slack", 1, "tok-b2", 1))
	if _, ok := w.table.botOwner("BB"); ok {
		t.Error("botOwner(BB) still names team-b after its tokens turned out to be another bot's")
	}
	if conn, ok := w.table.botOwner("BB2"); !ok || conn != "team-b/slack" {
		t.Errorf("botOwner(BB2) = %q, %v; want team-b", conn, ok)
	}
}

// The bot of a connection is forgotten when it is not served: it left the Snapshot, or it is a second connection of an app (its bot is the older
// one's, which the guard knows). A connection that is rejected, or whose start failed, is still served and keeps its bot.
func TestTheBotGuardForgetsTheBotOfAConnectionThatIsNotServed(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a"] = app
	w.ids["tok-c"] = appIdentity{App: "AC", Team: "T1", User: "UC", Bot: "BC"}
	w.ids["tok-d"] = appIdentity{App: "AD", Team: "T1", User: "UD", Bot: "BD"}
	w.ids["tok-b"] = appIdentity{App: "AAPP", Team: "T1", User: "UB2", Bot: "BB2"} // same app, other bot
	w.openErr["tok-d"] = errors.New("no adapter")
	a, b, c, d := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2), team("team-c/slack", 1, "tok-c", 3), team("team-d/slack", 1, "tok-d", 4)
	w.reconcile(a, b, c, d)
	if _, ok := w.table.botOwner("BB2"); ok {
		t.Error("botOwner(BB2) names team-b, which is refused as a second connection of team-a's app")
	}
	if conn, ok := w.table.botOwner("BD"); !ok || conn != "team-d/slack" {
		t.Errorf("botOwner(BD) = %q, %v; want team-d, which is served and failed to start", conn, ok)
	}

	w.reconcile(a, b, d) // team-c leaves the Snapshot
	if _, ok := w.table.botOwner("BC"); ok {
		t.Error("botOwner(BC) still names team-c, which is no longer served")
	}
	if conn, ok := w.table.botOwner("BAPP"); !ok || conn != "team-a/slack" {
		t.Errorf("botOwner(BAPP) = %q, %v; want team-a", conn, ok)
	}
}

// A connection that held no app because its start failed does not hold it through a restart either: the younger connection that serves the app in
// its place is not stopped for a lookup that did not answer.
func TestSupervisorKeepsNoAppForAHolderWhoseStartFailed(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-old"], w.ids["tok-young"] = app, app
	w.openErr["tok-old"] = errors.New("no adapter")
	young := team("team-b/slack", 1, "tok-young", 2)
	w.reconcile(team("team-a/slack", 1, "tok-old", 1), young)
	w.reconcile(team("team-a/slack", 1, "tok-old", 1), young)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Fatalf("running = %v; want the younger to serve the app the older cannot", got)
	}

	w.idErr["tok-old2"] = mayPass("auth.test: timed out")
	w.reconcile(team("team-a/slack", 1, "tok-old2", 1), young)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Errorf("running = %v; want the younger to go on serving while the older's new tokens are not known", got)
	}
	if got := w.sup.refusals.names(); len(got) != 0 {
		t.Errorf("refused = %v; want none", got)
	}
}

// mayPass is the error of an identity call that may succeed when it is asked again (identifyError marks them).
func mayPass(msg string) error { return mayPassError{errors.New(msg)} }

// A refused connection whose Secret is rotated to a token that Slack rejects is down for its credentials, and no longer refused: the refusal it
// was under does not come back at a look that cannot read its Secret, so its Bindings do not flip between ConnectionNotServed and
// ChatCredentialsRejected.
func TestSupervisorLiftsTheRefusalOfAConnectionWhoseNewTokenIsRejected(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a"], w.ids["tok-b1"] = app, app
	a, b1 := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b1", 2)
	w.reconcile(a, b1)
	if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Fatalf("refused = %v; want team-b", got)
	}

	w.idErr["tok-b2"] = &credentialsRejectedError{method: "auth.test", code: "invalid_auth"}
	b2 := team("team-b/slack", 1, "tok-b2", 2)
	for _, desired := range [][]TeamConnection{{a, b2}, {a, unready("team-b/slack", 1, 2)}, {a, b2}, {a, unready("team-b/slack", 1, 2)}} {
		w.reconcile(desired...)
		if got := w.sup.refusals.names(); len(got) != 0 {
			t.Fatalf("refused = %v; want none: team-b is down for its credentials, not refused", got)
		}
		var cs *connectionStopped
		if _, err := w.table.Platform("team-b/slack"); !errors.As(err, &cs) || !cs.rejected {
			t.Fatalf("Platform(team-b) = %v; want its credentials rejected as the reason it is down", err)
		}
	}
}

// A connection that Slack rejected holds no app: the younger connection of its app that is served is not stopped at a look that cannot read the
// rejected connection's Secret.
func TestSupervisorDoesNotStopTheYoungerConnectionWhenARejectedHoldersSecretIsUnreadable(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a1"], w.ids["tok-b"] = app, app
	a1, b := team("team-a/slack", 1, "tok-a1", 1), team("team-b/slack", 1, "tok-b", 2)
	w.reconcile(a1, b)

	w.idErr["tok-a2"] = &credentialsRejectedError{method: "auth.test", code: "token_revoked"}
	w.reconcile(team("team-a/slack", 1, "tok-a2", 1), b) // the holder rotates to a token Slack rejects
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Fatalf("running = %v; want the younger connection served in the place of the rejected holder", got)
	}
	w.reconcile(unready("team-a/slack", 1, 1), b)
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
		t.Errorf("running = %v with the rejected holder's Secret unreadable; want the younger connection not stopped", got)
	}
	if got := w.sup.refusals.names(); len(got) != 0 {
		t.Errorf("refused = %v; want none", got)
	}
}

// An answer that stays the same until the Secret changes (a missing scope, a token with no bot) does not keep a holder holding its app: the
// younger connection is served, for good, as it was before the holder's tokens changed. The same for a refused connection, which is not kept
// refused.
func TestSupervisorDoesNotHoldAnAppForAnAnswerThatDoesNotPass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a missing scope", errors.New(`bots.info: Slack answered "missing_scope"`)},
		{"a token with no bot", errors.New("auth.test: Slack gave no bot")},
		{"a bot with no app", errors.New("bots.info: Slack gave no app for the bot")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newSupWorld(t)
			app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
			w.ids["tok-a1"], w.ids["tok-b"] = app, app
			a1, b := team("team-a/slack", 1, "tok-a1", 1), team("team-b/slack", 1, "tok-b", 2)
			w.reconcile(a1, b)

			w.idErr["tok-a2"] = tc.err
			a2 := team("team-a/slack", 1, "tok-a2", 1)
			for range 20 {
				w.reconcile(a2, b)
			}
			if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
				t.Errorf("running = %v after 20 looks; want the younger connection serving the app the holder cannot be known to use", got)
			}
			if got := w.sup.refusals.names(); len(got) != 0 {
				t.Errorf("refused = %v; want none", got)
			}
			// Nor does the holder come back at a look that cannot read its Secret: it holds no app, and is not refused, so team-b is not touched.
			w.reconcile(unready("team-a/slack", 1, 1), b)
			if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
				t.Errorf("running = %v with team-a's Secret unreadable; want team-b still serving", got)
			}
			if got := w.sup.refusals.names(); len(got) != 0 {
				t.Errorf("refused = %v with team-a's Secret unreadable; want none", got)
			}

			// A refused connection whose tokens change to such an answer is not kept refused either.
			w.reconcile(unready("team-a/slack", 1, 1), b)
			w.idErr["tok-b2"] = tc.err
			w.ids["tok-a3"] = app
			a3 := team("team-a/slack", 1, "tok-a3", 1)
			w.reconcile(a3, b) // team-a serves the app again, team-b is refused
			if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
				t.Fatalf("refused = %v; want team-b refused again", got)
			}
			b2 := team("team-b/slack", 1, "tok-b2", 2)
			w.reconcile(a3, b2)
			if got := w.sup.refusals.names(); len(got) != 0 {
				t.Errorf("refused = %v; want the refusal of a connection whose new tokens give an answer that does not pass lifted", got)
			}
			w.reconcile(a3, unready("team-b/slack", 1, 2)) // and it does not come back at a look that cannot read the Secret
			if got := w.sup.refusals.names(); len(got) != 0 {
				t.Errorf("refused = %v with team-b's Secret unreadable; want none", got)
			}
		})
	}
}

// A pass that is cancelled while it asks whose bot the tokens are leaves the claims and the refusals as they were: the failure is the
// cancellation's, and not an answer about the tokens, so nothing is cleared and the younger connection is not served for the pass.
func TestSupervisorLeavesTheRefusalsAloneWhenThePassIsCancelledDuringALookup(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-a1"], w.ids["tok-b"] = app, app
	a1, b := team("team-a/slack", 1, "tok-a1", 1), team("team-b/slack", 1, "tok-b", 2)
	w.reconcile(a1, b)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	identify := w.sup.identify
	w.sup.identify = func(c context.Context, conn Connection) (appIdentity, error) {
		if conn.Credentials["botToken"].Reveal() == "tok-a2" {
			cancel()
			return appIdentity{}, errors.New(`bots.info: Slack answered "missing_scope"`) // as an answer that does not pass would be
		}
		return identify(c, conn)
	}
	w.sup.reconcile(ctx, []TeamConnection{team("team-a/slack", 1, "tok-a2", 1), b})
	if got := w.sup.refusals.names(); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Errorf("refused = %v after a pass cancelled during team-a's lookup; want team-b still refused", got)
	}
}
