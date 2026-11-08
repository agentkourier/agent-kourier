package broker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/identity"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// defaultSuperviseEvery is how often the supervisor looks at the team connections: the lag between a change that the config has taken in
// (a ChatConnection added, edited or taken away, or its Secret changed) and the runtime that follows it.
const defaultSuperviseEvery = 5 * time.Second

// supervisor keeps one runtime for each team ChatConnection the Snapshot serves (spec D51): it starts the ones that have none, restarts one whose
// ChatConnection or credentials changed (the fingerprint of TeamConnection), and stops the ones the Snapshot no longer serves. A connection whose
// Secret cannot be read yet is started once it can, and never fails the broker. The own connection is not its to start or stop: its tokens apply at a
// restart (spec D50 (6)), and it is only counted first when two connections are of one Slack app.
//
// Its loop is one goroutine, and everything in the struct but the host is its own. A failure of a team connection stops that connection only: its
// run ends, it leaves the table, and the table keeps why (a rejected credential, or a Secret that cannot be used), which the verification of its
// Bindings tells them; one WARN names the connection and the platform's code, agentkourier_chat_connected says 0 for it, and nothing else is touched.
// It runs again when its fingerprint changes, which is its Secret or its ChatConnection. A connection that leaves the Snapshot is gone: its metric
// series is deleted (removed).
type supervisor struct {
	host     *runtimeHost
	log      *slog.Logger
	every    time.Duration
	snapshot func() *config.Snapshot
	secrets  identity.SecretReader
	// own is the broker's own connection, whose identity comes first when two connections are of one Slack app.
	own      Connection
	identify func(ctx context.Context, conn Connection) (appIdentity, error)
	// intake builds the Sink of a connection's platform.
	intake   func(ref v1alpha1.ObjectRef) (chat.Sink, error)
	refusals *refusals
	// conns is where the supervisor publishes the connections the verification probes the Bindings of (see publish).
	conns *atomic.Pointer[[]Connection]
	// changes counts the passes that changed what conns holds, which the verification adds to the config's revision (see publish).
	changes *atomic.Uint64
	// gone is told the connection of a runtime that was stopped, so that the parts that keep state for it let go (the outbox's lane).
	gone func(conn string)
	// removed is told a connection that the Snapshot no longer serves: it is not restarting, it is away. The parts that keep a metric series per
	// connection delete it (Metrics.ConnectionRemoved). Nil is no one. Told once, after the runtime has stopped.
	removed func(conn string)
	// recover is told a connection whose runtime has started, so that the turns that were left while it did not run are taken up
	// (session.Manager.RecoverConnection). It is asked again at the next pass if it returned an error. Nil is no one.
	recover func(ctx context.Context, conn string) error

	done chan struct{}
	// passes counts the passes made, for a test that waits for one to be sure the supervisor has looked.
	passes   atomic.Int64
	ownID    appIdentity
	ownKnown bool
	// refusedNow is the connections the latest pass refused, for publish.
	refusedNow map[string]string
	// reasonsPub is why each connection was down when publish last ran, so that a change of it counts as a change of what is published.
	reasonsPub map[string]string
	teams      map[string]*teamRuntime
	said       map[string]string // the last line said of each connection, so that a state is said once
}

// teamRuntime is what the supervisor knows of one team connection.
type teamRuntime struct {
	// fp is the fingerprint of the ChatConnection and credentials this entry is for.
	fp string
	// id is the connection's bot, got for fp; asked says it was.
	id    appIdentity
	asked bool
	// rt is the running runtime, nil if there is none: it is refused, it failed to start, or the identity is not known yet.
	rt *connRuntime
	// failed says starting it failed for fp, which is not tried again until fp changes.
	failed bool
	// rejected says Slack refused the credentials of fp when the identity was asked: it is not asked again until fp changes. The table keeps it as
	// the reason the connection is down (runtimeTable.setDown), which is how the connection's Bindings are told (ChatCredentialsRejected).
	rejected bool
	// recovering says the turns the connection's runtime left are not taken up yet (supervisor.recover).
	recovering bool
	// refused says it was found to be a second connection of a Slack app, and not run for it. It stays refused while its credentials cannot be read.
	refused bool
}

// run supervises until ctx ends: it reconciles with initial, the team connections the takeover read, and then with what each tick finds. It
// returns when ctx has ended; the runtimes are left running, for the shutdown's haltAll. Every call that can block in a pass is bounded by ctx or by a
// timeout of its own, so once ctx has ended it returns within the longest of those.
func (s *supervisor) run(ctx context.Context, initial []TeamConnection) {
	defer close(s.done)
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	desired := initial
	for {
		s.reconcile(ctx, desired)
		s.passes.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		desired = teamConnections(ctx, s.snapshot(), s.own.Ref, s.secrets)
	}
}

// reconcile is one pass. It stops what is no longer served or has changed or is a second connection of an app, decides who holds each Slack app (the own
// connection first, then the team connections oldest first, since the order of desired is the age), and then starts what is to run, so that a younger
// connection of an app is stopped before the older one starts.
func (s *supervisor) reconcile(ctx context.Context, desired []TeamConnection) {
	s.refusedNow = nil
	defer func() { s.publish(desired) }()
	present := map[string]bool{}
	for _, t := range desired {
		present[t.Name()] = true
	}
	for name, st := range s.teams {
		if !present[name] {
			s.stop(ctx, name, st, "it is no longer served")
			s.host.table.clearDown(name)
			if s.removed != nil {
				s.removed(name)
			}
			delete(s.teams, name)
			delete(s.said, name)
		}
	}
	if len(desired) == 0 {
		s.refusals.set(nil)
		return
	}
	if !s.ownKnown {
		id, err := s.identify(ctx, s.own)
		if err != nil {
			s.say(ctx, "", "the identity of the own connection is not known yet, so no team connection starts", err)
			return
		}
		s.ownID, s.ownKnown = id, true
		s.host.table.identify(s.own.Ref.Namespace+"/"+s.own.Ref.Name, id) // the bot guard of the team connections tells the own bot from a person
		delete(s.said, "")
	}
	claimed := map[string]string{} // Slack app -> the connection that holds it
	if s.ownID.known() {
		claimed[s.ownID.App] = s.own.Ref.Namespace + "/" + s.own.Ref.Name
	}
	refused := map[string]string{}
	var toStart []string
	for _, t := range desired {
		name := t.Name()
		st := s.teams[name]
		if t.Err != nil {
			// Its Secret is not readable now, or its platform has no adapter. What runs keeps running on the tokens it was started with:
			// a cache that is relisting is no reason to drop a connection. Otherwise it starts once it can.
			if st != nil && st.rt != nil && st.id.known() {
				claimed[st.id.App] = name
			}
			if st != nil && st.refused {
				refused[name] = refusedMessage
			}
			if st == nil || st.rt == nil {
				s.say(ctx, name, "the ChatConnection is not started yet: its credentials cannot be used", t.Err)
				s.host.metrics.ChatConnected(name, false) // served and not connected: the series says so from the first look
				// A Secret that is gone, empty or of a type never read is for the operator to mend: its Bindings say so. A read that failed for a
				// moment (a cache that has not synced) is no finding. A rejection of the credentials it had stays, until they change.
				switch {
				case st != nil && st.rejected:
				case secretUnusable(t.Err):
					s.host.table.setDown(name, &connectionStopped{conn: name, cause: t.Err})
				default:
					s.host.table.clearUnusable(name)
				}
			}
			continue
		}
		if fp := t.fingerprint(); st == nil || st.fp != fp {
			if st != nil {
				s.stop(ctx, name, st, "its ChatConnection or its Secret changed")
			}
			st = &teamRuntime{fp: fp}
			s.teams[name] = st
			s.host.table.clearDown(name) // the credentials or the ChatConnection changed: whatever kept it down is for the new ones to show
			delete(s.said, name)
		} else {
			s.host.table.clearUnusable(name) // its Secret can be read now
		}
		if st.rejected {
			continue
		}
		if !st.asked {
			id, err := s.identify(ctx, t.Connection)
			if err != nil {
				if errors.Is(err, errCredentialsRejected) {
					st.rejected = true
					code, _ := rejectionCode(err)
					s.host.table.setDown(name, &connectionStopped{conn: name, rejected: true, code: code})
					s.host.metrics.ChatConnected(name, false)
					s.say(ctx, name, "the platform rejected the ChatConnection's credentials, so it is not started until its ChatConnection or its Secret changes", err)
					continue
				}
				s.say(ctx, name, "the identity of the ChatConnection's bot is not known yet, so it is not started", err)
				continue
			}
			st.id, st.asked = id, true
		}
		if st.id.known() {
			if holder, taken := claimed[st.id.App]; taken {
				st.refused = true
				refused[name] = refusedMessage
				s.stop(ctx, name, st, "it is a second connection of the Slack app that "+holder+" uses")
				s.say(ctx, name, "the ChatConnection is not served: it uses the same Slack app as the older ChatConnection "+holder+", and an app is served once", nil)
				continue
			}
			if st.rt != nil || !st.failed { // a connection whose start failed holds no app: a younger one of it may be served
				claimed[st.id.App] = name
			}
		}
		st.refused = false
		if st.rt == nil && !st.failed {
			toStart = append(toStart, name)
		}
	}
	s.refusals.set(refused)
	s.refusedNow = refused
	byName := map[string]TeamConnection{}
	for _, t := range desired {
		byName[t.Name()] = t
	}
	for _, name := range toStart {
		s.start(ctx, byName[name], s.teams[name])
	}
	for name, st := range s.teams {
		if st.recovering {
			s.recoverTurns(ctx, name, st)
		}
	}
}

// recoverTurns asks for the turns that were left while the connection did not run to be taken up, now that it runs, and asks again at the next
// pass if that failed. A connection that stopped again before it was asked keeps the question until its fingerprint changes and a new entry
// replaces this one.
func (s *supervisor) recoverTurns(ctx context.Context, name string, st *teamRuntime) {
	if s.recover == nil || st.rt == nil {
		st.recovering = false
		return
	}
	if ctx.Err() != nil || !s.host.table.runs(name) {
		return
	}
	if err := s.recover(ctx, name); err != nil {
		s.log.WarnContext(ctx, "chat connection: the turns it left while it did not run are not taken up yet; they are tried again", "connection", name, "error", telemetry.LogText(err))
		return
	}
	st.recovering = false
}

// start builds and runs the runtime of t.
func (s *supervisor) start(ctx context.Context, t TeamConnection, st *teamRuntime) {
	name := t.Name()
	if ctx.Err() != nil {
		return
	}
	rt, err := s.host.build(t.Connection, st.id)
	if errors.Is(err, errStopping) {
		return
	}
	if err != nil {
		st.failed = true
		s.say(ctx, name, "the ChatConnection cannot be started", err)
		return
	}
	sink, err := s.intake(t.Ref)
	if err != nil {
		s.host.stopRuntime(ctx, rt)
		st.failed = true
		s.say(ctx, name, "the ChatConnection cannot be started", err)
		return
	}
	st.rt = rt
	st.recovering = true
	s.host.run(rt, sink, s.ended)
	delete(s.said, name)
	s.log.InfoContext(ctx, "serving a ChatConnection", "connection", name)
}

// ended is told that the run of a team connection returned by itself: the platform rejected its credentials. The connection is down, and nothing else
// is touched; it runs again when its ChatConnection or its Secret changes.
func (s *supervisor) ended(rt *connRuntime, err error) {
	if rt.halted.Load() {
		return
	}
	code, _ := rejectionCode(err) // "" when the run ended for another reason, which only the broker's own stop is
	s.log.Warn("chat connection: the platform rejected its credentials, so its run ended and it is not served until its ChatConnection or its Secret changes", "connection", rt.name, "code", code, "error", telemetry.LogText(err))
}

// stop stops the runtime of st, if it has one, and says why. The connection is down from here.
func (s *supervisor) stop(ctx context.Context, name string, st *teamRuntime, why string) {
	if st.rt == nil {
		return
	}
	s.host.stopRuntime(ctx, st.rt)
	s.gone(name)
	st.rt = nil
	s.log.InfoContext(ctx, "chat connection stopped", "connection", name, "why", why)
}

// say logs a line about a connection once: the same line again for it says nothing. err, if any, is shown as a log text.
func (s *supervisor) say(ctx context.Context, name, msg string, err error) {
	line := msg
	if err != nil {
		line += ": " + telemetry.LogText(err)
	}
	if s.said[name] == line {
		return
	}
	s.said[name] = line
	if name == "" {
		s.log.WarnContext(ctx, msg, "error", telemetry.LogText(err))
		return
	}
	if err != nil {
		s.log.WarnContext(ctx, msg, "connection", name, "error", telemetry.LogText(err))
		return
	}
	s.log.WarnContext(ctx, msg, "connection", name)
}

// publish makes the list of connections the verification of the Bindings goes by: the own connection, with its credentials, and every team
// connection the Snapshot serves that the pass did not refuse, with the credentials of its runtime if one is running and none otherwise (a
// connection that is not running has no baseline to tell a changed Secret against). A refused connection is left out, so its Bindings are not
// probed: they are told they are not served. The list is built whole and never changed after it is stored, because the probes read it from
// several goroutines; nothing in them reads the supervisor's own state. It is called at the end of every pass, so it is as old as the last.
//
// A change of the list, which is a connection that started, restarted or stopped, or of why a connection is down (the table's reasons), counts in changes, and the verification adds the count to the
// revision of the config it watches: a rotated Secret moves the config's revision, so a pass runs while the connection still runs on the old tokens and
// tells its Bindings the credentials changed, and the restart that follows moves nothing the revision names, so without the count no pass
// would run until the next interval to clear it.
func (s *supervisor) publish(desired []TeamConnection) {
	if s.conns == nil {
		return
	}
	list := []Connection{s.own}
	for _, t := range desired {
		name := t.Name()
		if _, refused := s.refusedNow[name]; refused {
			continue
		}
		c := Connection{Ref: t.Ref, Platform: t.Platform}
		if st := s.teams[name]; st != nil && st.rt != nil && !isClosed(st.rt.done) {
			c = st.rt.conn
		}
		list = append(list, c)
	}
	reasons := s.host.table.reasons()
	if old := s.conns.Load(); old == nil || !sameConnections(*old, list) || !maps.Equal(reasons, s.reasonsPub) {
		s.conns.Store(&list)
		s.reasonsPub = reasons
		if s.changes != nil {
			s.changes.Add(1)
		}
	}
}

// sameConnections says whether a and b list the same connections with the same credentials, in the same order.
func sameConnections(a, b []Connection) bool {
	return slices.EqualFunc(a, b, func(x, y Connection) bool {
		return x.Ref == y.Ref && x.Platform == y.Platform && maps.EqualFunc(x.Credentials, y.Credentials, func(p, q secret.Value) bool { return p.Reveal() == q.Reveal() })
	})
}
