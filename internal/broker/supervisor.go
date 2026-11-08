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
// run ends, it leaves the table, and nothing else is told; it runs again when its fingerprint changes (failure isolation is not complete: the
// connection's Bindings are not yet told why, and its metric is not deleted).
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

	done chan struct{}
	// passes counts the passes made, for a test that waits for one to be sure the supervisor has looked.
	passes   atomic.Int64
	ownID    appIdentity
	ownKnown bool
	// refusedNow is the connections the latest pass refused, for publish.
	refusedNow map[string]string
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
	// rejected says Slack refused the credentials of fp when the identity was asked: it is not asked again until fp changes. MC-5 is where the
	// connection's Bindings are told (ChatCredentialsRejected); here it is the place where it is first known.
	rejected bool
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
			telemetry.ForgetConnection(ctx, name) // nothing of it runs now, so its series would only go stale
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
			}
			continue
		}
		if fp := t.fingerprint(); st == nil || st.fp != fp {
			if st != nil {
				s.stop(ctx, name, st, "its ChatConnection or its Secret changed")
			}
			st = &teamRuntime{fp: fp}
			s.teams[name] = st
			delete(s.said, name)
		}
		if st.rejected {
			continue
		}
		if !st.asked {
			id, err := s.identify(ctx, t.Connection)
			if err != nil {
				if errors.Is(err, errCredentialsRejected) {
					st.rejected = true
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
				// Not served from here, so the series of its earlier run would only go stale. Not only when this entry ran: a restart that
				// ends in a refusal (its Secret now holds another app's tokens) made this entry fresh, and the run it replaced left series.
				telemetry.ForgetConnection(ctx, name)
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
	s.log.Warn("chat connection: its run ended and it is not served until its ChatConnection or its Secret changes", "connection", rt.name, "error", telemetry.LogText(err))
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
// A change of the list, which is a connection that started, restarted or stopped, counts in changes, and the verification adds the count to the
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
	if old := s.conns.Load(); old == nil || !sameConnections(*old, list) {
		s.conns.Store(&list)
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
