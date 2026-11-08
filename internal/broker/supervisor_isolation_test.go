package broker

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// connectedSeries is the series of agentkourier_chat_connected of a connection, in the form samples gives.
func connectedSeries(conn string) string {
	return "agentkourier_chat_connected{connection=" + conn + "} "
}

// apps gives the connections the bot tokens of a world name distinct Slack apps.
func (w *supWorld) apps(tokens ...string) {
	for _, tok := range tokens {
		w.ids[tok] = appIdentity{App: "A-" + tok, Team: "T1", User: "U-" + tok, Bot: "B-" + tok}
	}
}

// opened returns the platform double built for the token.
func (w *supWorld) openedFor(token string) *fakeConn {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range w.opened {
		if f.token == token {
			return f
		}
	}
	w.t.Fatalf("no platform was built for %s", token)
	return nil
}

// stoppedBecause is what Platform(conn) says of a connection the platform rejected: an error that wraps chat.ErrConnectionDown and is a
// *connectionStopped, with the platform's code.
func (w *supWorld) stoppedBecause(conn string) *connectionStopped {
	w.t.Helper()
	_, err := w.table.Platform(conn)
	var stopped *connectionStopped
	if !errors.Is(err, chat.ErrConnectionDown) || !errors.As(err, &stopped) {
		w.t.Fatalf("Platform(%s) = %v; want a *connectionStopped that wraps chat.ErrConnectionDown", conn, err)
	}
	return stopped
}

// A team connection whose platform rejects the credentials is stopped, whatever finds it out: its run, or a call through any platform a part of
// the broker holds (a post of the outbox, a stream or an edit of the renderer, an edit, an ephemeral or a membership check of the interactions).
// Only that connection stops: the own connection and the other team connection run on and the broker does not exit. The metric says 0
// for it, one WARN names the connection and the platform's code and no token, its Bindings are told the credentials were rejected, and its
// direct calls fail fast with an error that wraps chat.ErrConnectionDown. It does not start again for a Secret that is as it was, and it
// starts again, working, for one that changed.
func TestATeamConnectionThePlatformRejectsStopsThatConnectionOnly(t *testing.T) {
	for _, call := range []string{"Run", "Post", "Stream", "Update", "Ephemeral", "IsMember"} {
		t.Run(call, func(t *testing.T) {
			w := newSupWorld(t)
			w.apps("tok-a", "tok-b", "tok-a2")
			a, b := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2)
			w.reconcile(a, b)
			w.eventually("three runs", func() bool { return w.live.Load() == 3 })
			if got := samples(t, w.host.metrics); !slices.Contains(got, connectedSeries("team-a/slack")+"1") {
				t.Fatalf("samples = %v; want team-a connected", got)
			}

			if call == "Run" {
				close(w.openedFor("tok-a").reject)
			} else {
				w.openedFor("tok-a").failWith(call, errRejected)
				p, err := w.table.Platform("team-a/slack")
				if err != nil {
					t.Fatal(err)
				}
				switch call {
				case "Post":
					_, err = p.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Outgoing{}, "id")
				case "Stream":
					_, err = p.Stream(t.Context(), chat.Target{Channel: "C1"}, "U1", "hi")
				case "Update":
					err = p.Update(t.Context(), chat.Ref{Channel: "C1", ID: "1.1"}, chat.Outgoing{})
				case "Ephemeral":
					err = p.Ephemeral(t.Context(), chat.Target{Channel: "C1"}, "U1", chat.Message{})
				case "IsMember":
					_, err = p.IsMember(t.Context(), "C1", "U1")
				}
				if !errors.Is(err, errRejected) {
					t.Fatalf("%s = %v; want the platform's own error back", call, err)
				}
				// From the call that found out, the connection fails fast, before its run has ended and left the table.
				if _, err := w.table.Platform("team-a/slack"); !errors.Is(err, chat.ErrConnectionDown) {
					t.Errorf("Platform(team-a) right after the call = %v; want it down", err)
				}
			}

			w.eventually("team-a to leave the table", func() bool { return !slices.Contains(w.running(), "team-a/slack") })
			if got := w.running(); !slices.Equal(got, []string{ownName, "team-b/slack"}) {
				t.Errorf("running = %v; want the own connection and team-b, which are not touched", got)
			}
			if stopped := w.stoppedBecause("team-a/slack"); !stopped.rejected || stopped.code != "invalid_auth" {
				t.Errorf("stopped = %+v; want the platform's rejection, with its code", stopped)
			}
			if _, err := w.table.Platform("team-b/slack"); err != nil {
				t.Errorf("Platform(team-b) = %v; want its platform", err)
			}
			w.eventually("the metric to say 0 for team-a", func() bool {
				return slices.Contains(samples(t, w.host.metrics), connectedSeries("team-a/slack")+"0")
			})
			if got := samples(t, w.host.metrics); !slices.Contains(got, connectedSeries("team-b/slack")+"1") || !slices.Contains(got, connectedSeries(ownName)+"1") {
				t.Errorf("samples = %v; want the other connections still connected", got)
			}
			w.eventually("the WARN", func() bool {
				return strings.Contains(w.logs.String(), "the platform rejected its credentials, so its run ended")
			})
			logged := w.logs.String()
			for _, want := range []string{"connection=team-a/slack", "code=invalid_auth"} {
				if !strings.Contains(logged, want) {
					t.Errorf("the log lacks %q:\n%s", want, logged)
				}
			}
			if strings.Contains(logged, "tok-a") {
				t.Errorf("the log holds a token:\n%s", logged)
			}

			w.reconcile(a, b) // unchanged: it is not started again for nothing
			if got := w.tokens(); !slices.Equal(got, []string{"own-token", "tok-a", "tok-b"}) {
				t.Errorf("platforms built = %v; want no restart of a rejected connection whose Secret is as it was", got)
			}
			_ = w.stoppedBecause("team-a/slack") // and still says why

			w.reconcile(team("team-a/slack", 1, "tok-a2", 1), b) // its Secret was replaced
			w.eventually("team-a to run again", func() bool { return slices.Contains(w.running(), "team-a/slack") })
			if _, err := w.table.Platform("team-a/slack"); err != nil {
				t.Errorf("Platform(team-a) after its Secret changed = %v; want its platform", err)
			}
			w.eventually("the metric to say 1 for team-a again", func() bool {
				return slices.Contains(samples(t, w.host.metrics), connectedSeries("team-a/slack")+"1")
			})
		})
	}
}

// A connection whose token Slack refuses when its bot is asked about (auth.test or bots.info) is not started, and its Bindings are told the
// credentials were rejected, as for a run that found out; a new token is asked about, and the connection starts.
func TestATeamConnectionWhoseBotSlackRefusesIsToldToItsBindings(t *testing.T) {
	w := newSupWorld(t)
	w.idErr["tok-a"] = &credentialsRejectedError{method: "bots.info", code: "token_revoked"}
	w.apps("tok-a2")

	w.reconcile(team("team-a/slack", 1, "tok-a", 1))
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Fatalf("running = %v; want the connection not to start", got)
	}
	if stopped := w.stoppedBecause("team-a/slack"); !stopped.rejected || stopped.code != "token_revoked" {
		t.Errorf("stopped = %+v; want the rejection, with the code of bots.info", stopped)
	}
	if got := samples(t, w.host.metrics); !slices.Contains(got, connectedSeries("team-a/slack")+"0") {
		t.Errorf("samples = %v; want 0 for team-a: it never came up", got)
	}

	w.reconcile(team("team-a/slack", 1, "tok-a2", 1))
	if got := w.running(); !slices.Equal(got, []string{ownName, "team-a/slack"}) {
		t.Errorf("running = %v with a new token; want it to start", got)
	}
	if _, err := w.table.Platform("team-a/slack"); err != nil {
		t.Errorf("Platform(team-a) = %v; want its platform: the rejection is over", err)
	}
}

// A team connection whose Secret is gone, empty or never read is left out, and says why to its Bindings (Platform tells it); a Secret that cannot
// be read just now (a cache that is relisting) says nothing of the kind. It starts once the Secret can be used, and a connection that runs keeps
// running when its Secret is gone.
func TestATeamConnectionWhoseSecretCannotBeUsedIsLeftOutAndSaysWhy(t *testing.T) {
	w := newSupWorld(t)
	w.apps("tok-a", "tok-b")
	unusable := func(name string, err error) TeamConnection {
		u := unready(name, 1, 1)
		u.Err = fmt.Errorf("ChatConnection %s: read botToken of Secret %s: %w", name, name, err)
		return u
	}

	w.reconcile(unready("team-a/slack", 1, 1))
	if _, err := w.table.Platform("team-a/slack"); !errors.Is(err, chat.ErrConnectionDown) || errors.As(err, new(*connectionStopped)) {
		t.Errorf("Platform(team-a) with its Secret not synced = %v; want plain down: that is no finding", err)
	}

	for _, cause := range []error{secret.ErrKeyNotFound, secret.ErrEmptyValue, secret.ErrUnsupportedType} {
		w.reconcile(unusable("team-a/slack", cause))
		if stopped := w.stoppedBecause("team-a/slack"); stopped.rejected || !errors.Is(stopped.cause, cause) {
			t.Errorf("stopped = %+v; want the Secret as unusable, because %v", stopped, cause)
		}
	}
	if got := w.running(); !slices.Equal(got, []string{ownName}) {
		t.Errorf("running = %v; want the own connection alone", got)
	}

	w.reconcile(team("team-a/slack", 1, "tok-a", 1)) // mended
	if _, err := w.table.Platform("team-a/slack"); err != nil {
		t.Errorf("Platform(team-a) once its Secret is mended = %v; want its platform", err)
	}

	w.reconcile(unusable("team-a/slack", secret.ErrKeyNotFound)) // gone again, while it runs
	if _, err := w.table.Platform("team-a/slack"); err != nil {
		t.Errorf("Platform(team-a) with its Secret gone while it runs = %v; want it to run on the tokens it has", err)
	}
}

// A connection that leaves the Snapshot is removed: its agentkourier_chat_connected series is deleted, not set to 0, and the supervisor says
// so to the hook other series are deleted by. One that restarts keeps its series, and so does one that is only stopped. A report of the adapter's
// connection after the runtime was stopped does not bring the series back.
func TestARemovedConnectionsMetricSeriesIsDeleted(t *testing.T) {
	w := newSupWorld(t)
	w.apps("tok-a", "tok-a2", "tok-b")
	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))
	w.eventually("three runs", func() bool { return w.live.Load() == 3 })
	late := w.openedFor("tok-a").onConn // the adapter's callback, which can fire after the runtime was stopped

	w.reconcile(team("team-a/slack", 1, "tok-a2", 1), team("team-b/slack", 1, "tok-b", 2)) // a restart
	w.eventually("the restarted connection to be up", func() bool {
		return slices.Contains(samples(t, w.host.metrics), connectedSeries("team-a/slack")+"1")
	})
	late(true) // the old run's connection reports up, late
	late(false)
	if got := samples(t, w.host.metrics); !slices.Contains(got, connectedSeries("team-a/slack")+"1") {
		t.Errorf("samples = %v; a late report of the replaced runtime changed the series of the restarted connection", got)
	}
	if len(w.removed) != 0 {
		t.Errorf("removed = %v after a restart; want none", w.removed)
	}

	w.reconcile(team("team-b/slack", 1, "tok-b", 2)) // team-a is gone
	got := samples(t, w.host.metrics)
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, connectedSeries("team-a/slack")) }) {
		t.Errorf("samples = %v; want the series of the removed connection deleted", got)
	}
	if !slices.Contains(got, connectedSeries("team-b/slack")+"1") || !slices.Contains(got, connectedSeries(ownName)+"1") {
		t.Errorf("samples = %v; want the other connections' series kept", got)
	}
	late(true)
	if got := samples(t, w.host.metrics); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, connectedSeries("team-a/slack")) }) {
		t.Errorf("samples = %v; a late report brought the removed connection's series back", got)
	}
	w.mu.Lock()
	removed := slices.Clone(w.removed)
	w.mu.Unlock()
	if !slices.Equal(removed, []string{"team-a/slack"}) {
		t.Errorf("removed = %v; want the removed connection, once", removed)
	}

	// A connection that never ran and is removed has its series deleted too (it was set to 0 when its start was tried).
	w.openErr["tok-c"] = errors.New("no adapter")
	w.reconcile(team("team-b/slack", 1, "tok-b", 2), team("team-c/slack", 1, "tok-c", 3))
	w.reconcile(team("team-b/slack", 1, "tok-b", 2))
	if got := samples(t, w.host.metrics); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, connectedSeries("team-c/slack")) }) {
		t.Errorf("samples = %v; want the series of a removed connection that never ran deleted", got)
	}
}

// A connection that stops for its credentials keeps its series at 0: it is still in the Snapshot.
func TestAStoppedConnectionKeepsItsMetricSeries(t *testing.T) {
	w := newSupWorld(t)
	w.apps("tok-a")
	w.reconcile(team("team-a/slack", 1, "tok-a", 1))
	w.eventually("two runs", func() bool { return w.live.Load() == 2 })
	close(w.openedFor("tok-a").reject)
	w.eventually("the series to say 0", func() bool {
		return slices.Contains(samples(t, w.host.metrics), connectedSeries("team-a/slack")+"0")
	})
	w.reconcile(team("team-a/slack", 1, "tok-a", 1))
	if got := samples(t, w.host.metrics); !slices.Contains(got, connectedSeries("team-a/slack")+"0") {
		t.Errorf("samples = %v; want the stopped connection's series kept at 0", got)
	}
}

// The turns a connection left while it did not run are asked to be taken up when it runs, once, and again at the next pass if that failed. A
// connection that never runs is never asked for.
func TestTheSupervisorAsksForTheTurnsOfAConnectionOnceItRuns(t *testing.T) {
	w := newSupWorld(t)
	w.apps("tok-a", "tok-b")
	w.idErr["tok-b"] = &credentialsRejectedError{method: "auth.test", code: "invalid_auth"}
	w.recErr = errors.New("the store is busy")

	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))
	w.mu.Lock()
	w.recErr = nil
	w.mu.Unlock()
	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))
	w.reconcile(team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2))

	w.mu.Lock()
	defer w.mu.Unlock()
	if want := []string{"team-a/slack", "team-a/slack"}; !slices.Equal(w.recov, want) {
		t.Errorf("asked for the turns of %v; want %v: the failed ask repeated once, and the connection that never ran not at all", w.recov, want)
	}
}

// A connection whose Secret cannot be read has no runtime and no entry of its own in the supervisor, yet it has a series and a reason in the table: when
// it leaves the Snapshot both go, or an alert on chat_connected == 0 fires for a connection that is not there.
func TestAConnectionWhoseSecretWasNeverThereLeavesNoSeriesOrReasonWhenRemoved(t *testing.T) {
	w := newSupWorld(t)
	typo := unready("team-c/slack", 1, 1)
	typo.Err = fmt.Errorf("ChatConnection team-c/slack: read botToken of Secret team-c/creds: %w", config.ErrSecretNotFound)
	w.reconcile(typo)
	if got := samples(t, w.host.metrics); !slices.Contains(got, connectedSeries("team-c/slack")+"0") {
		t.Fatalf("samples = %v; want a series at 0 for the served connection that cannot start", got)
	}
	_ = w.stoppedBecause("team-c/slack")

	w.reconcile()
	if got := samples(t, w.host.metrics); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, connectedSeries("team-c/slack")) }) {
		t.Errorf("samples = %v; want the series of the removed connection deleted", got)
	}
	if reasons := w.table.reasons(); len(reasons) != 0 {
		t.Errorf("the table still says why %v are down", reasons)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.Equal(w.removed, []string{"team-c/slack"}) {
		t.Errorf("removed = %v; want the connection, once", w.removed)
	}
	if len(w.sup.seen) != 0 {
		t.Errorf("the supervisor still remembers %v", w.sup.seen)
	}
}

// A platform resolved before the connection was rejected, which a turn holds for its length, fails fast after it: the error is the typed one, and
// the call is not made on the platform. So does one resolved before the connection restarted, whose transport was closed.
func TestAPlatformResolvedBeforeTheConnectionStoppedFailsFastAfter(t *testing.T) {
	w := newSupWorld(t)
	w.apps("tok-a", "tok-a2")
	w.reconcile(team("team-a/slack", 1, "tok-a", 1))
	w.eventually("two runs", func() bool { return w.live.Load() == 2 })
	held, err := w.table.Platform("team-a/slack") // as the renderer resolves it, once per turn
	if err != nil {
		t.Fatal(err)
	}
	old := w.openedFor("tok-a")
	old.failWith("Post", errRejected)
	if _, err := held.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Outgoing{}, "id"); !errors.Is(err, errRejected) {
		t.Fatalf("Post = %v; want the platform's own error", err)
	}

	for _, call := range []func() error{
		func() error {
			_, err := held.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Outgoing{}, "id")
			return err
		},
		func() error { return held.Update(t.Context(), chat.Ref{Channel: "C1", ID: "1.1"}, chat.Outgoing{}) },
		func() error { _, err := held.Stream(t.Context(), chat.Target{Channel: "C1"}, "U1", "hi"); return err },
		func() error { return held.StreamAt(chat.Ref{Channel: "C1", ID: "1.1"}).Append(t.Context(), "more") },
	} {
		var stopped *connectionStopped
		if err := call(); !errors.Is(err, chat.ErrConnectionDown) || !errors.As(err, &stopped) || !stopped.rejected {
			t.Errorf("a call on the held platform after the rejection = %v; want the typed error", err)
		}
	}
	if n := old.called("Post"); n != 1 {
		t.Errorf("the platform saw %d posts; want only the one that found the rejection", n)
	}
	if n := old.called("Update") + old.called("Stream") + old.called("Append"); n != 0 {
		t.Errorf("the platform saw %d more calls after the rejection; want none", n)
	}

	// A restart: the platform held from before it fails fast, and the new one works.
	w.reconcile(team("team-a/slack", 1, "tok-a2", 1))
	w.eventually("team-a to run again", func() bool { return w.live.Load() == 2 && slices.Contains(w.running(), "team-a/slack") })
	held2, err := w.table.Platform("team-a/slack")
	if err != nil {
		t.Fatal(err)
	}
	w.reconcile(team("team-a/slack", 2, "tok-a2", 1)) // restarted again
	w.eventually("the restart", func() bool { return len(w.tokens()) == 4 })
	prev := w.openedFor("tok-a2")
	if err := held2.Update(t.Context(), chat.Ref{Channel: "C1", ID: "1.1"}, chat.Outgoing{}); !errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("a call on the platform of the replaced runtime = %v; want it down", err)
	}
	if n := prev.called("Update"); n != 0 {
		t.Errorf("the replaced runtime's platform saw %d calls after it was closed; want none", n)
	}
}

// A connection stopped because an older connection of its Slack app turned up is not served, and its agentkourier_chat_connected series is deleted,
// once, and not brought back by the passes that find it refused again, even with its Secret unreadable meanwhile. So is one that restarts into a
// refusal. The older connection's series stays.
func TestARefusedConnectionsChatConnectedSeriesIsDeleted(t *testing.T) {
	w := newSupWorld(t)
	app := appIdentity{App: "AAPP", Team: "T1", User: "UAPP", Bot: "BAPP"}
	w.ids["tok-old"], w.ids["tok-young"] = app, app
	has := func(conn string) bool {
		return slices.ContainsFunc(samples(t, w.host.metrics), func(s string) bool { return strings.HasPrefix(s, connectedSeries(conn)) })
	}
	old, young := team("team-a/slack", 1, "tok-old", 1), team("team-b/slack", 1, "tok-young", 2)

	w.reconcile(young)
	w.eventually("the younger connection to run", func() bool { return w.live.Load() == 2 })
	if !has("team-b/slack") {
		t.Fatal("the running connection has no series")
	}
	w.reconcile(old, young) // the older connection of the app turns up
	if has("team-b/slack") {
		t.Errorf("samples = %v; want the series of the refused connection deleted", samples(t, w.host.metrics))
	}
	if !has("team-a/slack") || !has(ownName) {
		t.Errorf("samples = %v; want the served connections' series kept", samples(t, w.host.metrics))
	}
	unreadable := unready("team-b/slack", 1, 2)
	w.reconcile(old, unreadable) // refused, and its Secret cannot be read just now
	w.reconcile(old, young)
	if has("team-b/slack") {
		t.Errorf("samples = %v; a pass that found the refused connection again brought its series back", samples(t, w.host.metrics))
	}
	w.mu.Lock()
	removed := slices.Clone(w.removed)
	w.mu.Unlock()
	if !slices.Equal(removed, []string{"team-b/slack"}) {
		t.Errorf("removed = %v; want the refused connection once, and not the ones that restarted or were rejected", removed)
	}

	// A connection that restarts into a refusal: its Secret now holds the tokens of the older connection's app.
	w2 := newSupWorld(t)
	w2.ids["tok-a"], w2.ids["tok-b"], w2.ids["tok-b2"] = appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}, appIdentity{App: "AB", Team: "T1", User: "UB", Bot: "BB"}, appIdentity{App: "AA", Team: "T1", User: "UA", Bot: "BA"}
	a, b := team("team-a/slack", 1, "tok-a", 1), team("team-b/slack", 1, "tok-b", 2)
	w2.reconcile(a, b)
	w2.eventually("both connections to run", func() bool { return w2.live.Load() == 3 })
	w2.reconcile(a, team("team-b/slack", 2, "tok-b2", 2))
	if slices.ContainsFunc(samples(t, w2.host.metrics), func(s string) bool { return strings.HasPrefix(s, connectedSeries("team-b/slack")) }) {
		t.Errorf("samples = %v; want the series of the connection that restarted into a refusal deleted", samples(t, w2.host.metrics))
	}
}
