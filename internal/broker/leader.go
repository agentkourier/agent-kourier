package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// leaderHalf is what Agent Kourier runs only as the leader (spec 11, D37): the Binding pool and the chat platform, the
// outbox, the session manager and its recovery, the sweeps and digests, the interactions' loops and the chat
// connection (Socket Mode). With SQLite, or with Postgres once a replica holds the lease, Run starts it, in the
// order a start-up has always had; what every replica runs (the health server, the config and the Secret caches,
// the store) is Run's own and is already up. Each part registers its stop with the shutdown as it starts.
type leaderHalf struct {
	cfg     RunConfig
	log     *slog.Logger
	sd      *shutdown
	metrics *Metrics
	boot    *Config
	st      openedStore
	// leaseFence refuses the chat writes, and the sends, answers and cancels to the agent, of a leader whose lease is past its
	// renew deadline. Nil with SQLite, which has no lease: openPlatform is the one place the platform is built, and start the one
	// place the Bindings are, so that no write to the chat and no call that changes the agent goes round it.
	leaseFence *leaseFence
	// status is what the status writer writes with, set only with the crds config source: nil means the config comes from
	// files, whose Bindings have no status, and the leader half makes no writer.
	status *statusSink
	// connsServed is the connections served, as the supervisor publishes them: Run made it, and its metrics read it (see Run).
	connsServed *atomic.Pointer[[]Connection]
}

// openPlatform builds the chat platform of conn, and with an election fences it: every write to the chat that the leader half
// makes (the outbox's posts and reactions, the renderer's streams and edits, the interactions' prompts, edits and ephemerals)
// goes through the platform that this returns, and nothing else builds one.
func (h *leaderHalf) openPlatform(conn Connection, onConnection func(connected bool)) (builtPlatform, error) {
	p, err := h.cfg.newPlatform(conn, h.log, onConnection)
	if err != nil {
		return builtPlatform{}, err
	}
	if h.leaseFence != nil {
		p.Platform = fenceChat(p.Platform, h.leaseFence)
	}
	return p, nil
}

// start starts the leader half on the connections as they are now (a standby that leads hours after it started must not use
// the tokens it saw at start: a takeover is a restart to the new leader): the own connection's runtime, and a supervisor that starts
// the team connections' (supervisor), the first of them from the ones read with the own. Its parts run on
// runCtx (the broker's own, which only the shutdown ends; ctx is Run's, the signal, which it only logs with). It returns the channel that says how the own
// connection's run ended: a team connection's run ends nothing but itself. An error
// from a step is returned as it is; whatever started before it has registered its stop. It must not be called twice, or
// once a shutdown has begun (Run's startGate sees to both).
func (h *leaderHalf) start(ctx, runCtx context.Context, chosen Connections) (<-chan error, error) {
	cfg, log, sd, metrics, boot, st := h.cfg, h.log, h.sd, h.metrics, h.boot, h.st
	conn := chosen.Own
	connName := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	var auditLog store.Audit = st
	if cfg.auditStore != nil {
		auditLog = cfg.auditStore(st)
	}
	auditor, err := audit.New(auditLog, audit.Options{Logger: log, OnFailure: metrics.AuditFailure})
	if err != nil {
		return nil, err
	}
	// The chat platform of the own connection, built for its ChatConnection.platform: it posts for the outbox, and a token revoked
	// mid-run, which a call through the platform sees, ends the run with ErrSlackAuth (WatchAuth, WithAuthFailure): the process exits for the own
	// connection, and only that connection stops for a team's.
	// agentkourier_chat_connected says 0 from here, and 1 while the connection is up. /readyz does not hold it: see NewHandler.
	// The parts that every connection shares look the platform up by the connection their row, turn or press names, in the table of the
	// connections that run, which the supervisor adds the team connections' to and takes them from.
	table := newRuntimeTable()
	host := &runtimeHost{cfg: cfg, log: log, metrics: metrics, table: table, open: h.openPlatform, parent: runCtx}
	ownRT, err := host.build(conn)
	if err != nil {
		return nil, err
	}
	var platforms chat.Resolver[chat.Platform] = table
	// The Bindings take the acknowledgment reaction of the platform of their own connection, so the platform is built first.
	bindings, err := NewBindings(boot.Reloader, boot.Secrets, auditor, Options{
		OnRejected: metrics.CredentialRejected, Platforms: chat.Narrow[platformCaps](platforms), fence: h.leaseFence})
	if err != nil {
		host.closeAll()
		return nil, err
	}
	sd.add(stepPool, func(context.Context) { bindings.Close() })

	// The rows that were written before connections were recorded are this connection's, and are named so before the outbox
	// reconciles or a session recovers: nothing reads a row that belongs to nobody.
	if err := attributeStoredRows(runCtx, st, log, connName); err != nil {
		return nil, err
	}
	ob, err := outbox.New(outbox.Config{
		Store: st, Platforms: table.outbox(), Logger: log, OnAbandon: metrics.OutboxAbandoned,
		// A row whose producer names no connection is the own connection's.
		DefaultConnection: connName,
	})
	if err != nil {
		return nil, err
	}
	if cfg.outboxBuilt != nil {
		cfg.outboxBuilt(ob)
	}
	if err := ob.Reconcile(runCtx); err != nil { // Slack may be down: Run below tries again, so this only says so
		log.WarnContext(ctx, "outbox: the first reconcile did not finish; it will be tried again", "error", err)
	}
	outboxCtx, stopOutbox := context.WithCancel(runCtx)
	outboxDone := make(chan struct{})
	go func() { defer close(outboxDone); _ = ob.Run(outboxCtx) }()
	sd.add(stepDrain, func(c context.Context) { // stop the drain loop, then post what is pending one last time
		stopOutbox()
		waitFor(c, outboxDone)
		if err := ob.Reconcile(c); err != nil && c.Err() == nil {
			log.Warn("outbox: the final drain did not finish", "error", err)
		}
		// Slack's connections close only now: the drain posts through them, and a connection that its
		// calls opened after an earlier close would be one that nothing closes. Every connection that still runs has its own.
		host.closeAll()
	})

	// The renderer posts a turn's first message through the outbox and edits or streams it through the
	// chat platform (the Slack adapter over the client); a notice is only queued on the outbox, off Slack's
	// acknowledgement path.
	renderer, err := live.New(live.Config{Sender: ob, Audit: auditor, Platforms: platforms, Store: st, Logger: log})
	if err != nil {
		return nil, err
	}
	var sink renderSink = live.NewSink(renderer, ob, log)
	if cfg.sink != nil {
		sink = cfg.sink
	}
	// The manager's sink is the tap, which hands the pauses of its turns to the interactions' intake; the
	// intake answers through the manager, so its session is bound once the manager is built (D2).
	// Whatever the interactions start adds its stop to stops, which runs between the intake, which feeds
	// them, and the manager, which they feed.
	stops := stopList{trace: cfg.stopTrace}
	sd.add(stepInteractions, stops.run)
	parts := interactionDeps{st: st, ob: ob, platforms: platforms, auditor: auditor, bindings: bindings, metrics: metrics, log: log, trace: cfg.startTrace, sweepEvery: cfg.sweepEvery, orphanGrace: cfg.orphanGrace}
	ix, err := parts.build(sink)
	if err != nil {
		return nil, err
	}
	// A mentioned turn of a mention-only Binding carries the replies since the previous turn, which are read through the
	// platform as it is built here (fenced with an election: the read is not a write, so the fence passes it).
	threads, err := newThreadContext(threadContextConfig{Platforms: platforms, Replies: st, Logger: log})
	if err != nil {
		return nil, err
	}
	// A turn's acknowledgment removal is queued and its reply settled at once, so that retention does not queue it again.
	unack := unacknowledge(sink.QueueNotice, st.SettleAcknowledgmentReplies)
	mgr, err := session.New(session.Config{Context: runCtx, ConnectionRunning: table.runs, Store: st, Questions: st, Bindings: bindings, Users: users{platforms}, Sink: ix.tap, TextReply: ix.replies.Decide, Logger: log, Audit: auditor, OnTurnEnd: metrics.TurnOutcome, ThreadContext: threads.Section, AnswerWatcher: ix.reopened, Unacknowledge: unack})
	if err != nil {
		return nil, fmt.Errorf("session manager: %w", err)
	}
	ix.late.bind(mgr)
	sd.add(stepManager, func(c context.Context) {
		if err := mgr.Close(c); err != nil {
			log.Warn("closing the session manager", "error", err)
		}
		// The renders that were writing a final text when their turns ended are waited for here, after the
		// turns, and before the drain that stops the outbox they post through.
		if err := renderer.Wait(c); err != nil {
			log.Warn("waiting for the renders of the last turns", "error", err)
		}
	})
	if err := parts.recovered("recover-sessions", func() error { return mgr.Recover(runCtx) }); err != nil {
		return nil, fmt.Errorf("recover sessions: %w", err)
	}
	// The rows of old trigger threads are purged on a timer. It writes only to the store, so it stops with the
	// interactions' loops, before the store closes; added first, it stops last, after the tap and the rest.
	sweep, err := newThreadSweep(threadSweepConfig{Store: st, Interval: cfg.threadSweepEvery, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("trigger thread sweep: %w", err)
	}
	stops.add("trigger-threads", startLoop(runCtx, sweep.Run))
	// The digest line of the alerts that a trigger's limits stopped is queued on the outbox, one for each window: the
	// outbox posts it. The loop stops with the interactions' loops, before the outbox drain and the store.
	digest, err := newTriggerDigest(triggerDigestConfig{Store: st, Queue: ob, Interval: cfg.digestEvery, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("trigger digest: %w", err)
	}
	stops.add("trigger-digest", startLoop(runCtx, digest.Run))
	// The retention of what no other loop owns (data protection): reply text that was never delivered, tasks that a crash
	// left on sessions that have been expired for good, those sessions with what belongs to them, and audit entries past
	// the operator's retention, if one is set. It writes to the store and the audit, and asks an agent to cancel a stale
	// task, a call that is bounded and that its context ends, so it stops with the other loops.
	// With the custom resources the record of the connections served (the status writer's, below) also tells the retention which Bindings
	// belong to a connection this broker served before and does not now: their sessions are kept like those of any unserved Binding.
	var served *servedRecord
	retentionKnows := bindings.ThreadTTLsAndUnserved
	if h.status != nil && boot.Source != nil {
		served = newServedRecord(st, boot.Source.ConnectionBindings, nil)
		retentionKnows = served.retentionKnows(runCtx, boot.Reloader.Report, log, retentionKnows)
	}
	retain, err := newRetention(retentionConfig{Store: st, Interval: cfg.retentionEvery, AuditRetention: cfg.AuditRetention, ThreadTTLsAndUnserved: retentionKnows,
		Canceller: bindings, Audit: auditor, Holds: mgr.HoldsTask, OnRemoved: metrics.RetentionRemoved, Logger: log,
		EnsureAcknowledgment: func(ctx context.Context, r store.Reply) error {
			return sink.QueueNotice(ctx, r.Key, session.Notice{ID: session.AcknowledgmentID(r.ID), Kind: session.NoticeAcknowledged, MessageRef: r.MessageRef, Reaction: r.Acknowledgment})
		},
		ClearAcknowledgment: func(ctx context.Context, r store.Reply) error {
			return sink.QueueNotice(ctx, r.Key, session.Notice{ID: session.AcknowledgmentID(r.ID), Kind: session.NoticeUnacknowledged, MessageRef: r.MessageRef, Reaction: r.Acknowledgment})
		}})
	if err != nil {
		return nil, fmt.Errorf("store retention: %w", err)
	}
	stops.add("store-retention", startLoop(runCtx, retain.Run))
	// The verification of the Bindings: whether each one's backend answers and its channel has the bot in it, probed on a schedule
	// and reported as metrics and log lines only. It sends the agent nothing, posts nothing and changes no readiness. It reads
	// through the Bindings' pools, the platform and the Secrets, so it stops in the interactions' step, before the manager, the drain, the pool
	// and the resolver. Added before the interactions start, it stops after their tap, worker and sweeper, with the store's loops.
	// With the crds config source the leader also writes what the Prober found, and what the source refused, into each Binding's
	// status (the status writer, which a standby never has, and the files route has none). It runs once the Prober exists, since it
	// reads its State, and is stopped after it, since it reads that State to the end.
	var prober *bindingcheck.Prober
	var statusW *statusWriter
	// The connections the supervisor does not run because they are a second connection of one Slack app are in the Snapshot, and so are
	// their Bindings: the report that the writer reads says those Bindings are not served (refusals.overlay).
	refused := &refusals{}
	parts.served = refused.served // the timeout sweep ends the interactions of a refused connection without the agent
	// The connections the verification probes the Bindings of: the supervisor publishes the list after each of its passes (supervisor.publish),
	// and until the first the own connection's alone.
	connsServed := h.connsServed
	connsServed.Store(&[]Connection{conn})
	var connsChanged atomic.Uint64
	if h.status != nil {
		if boot.Source == nil {
			return nil, errors.New("binding status: the config has no custom resource source to read the statuses from")
		}
		statusW, err = newStatusWriter(statusConfig{
			Client: h.status.client, Report: func() config.Report { return refused.overlay(boot.Reloader.Report()) }, Conditions: func() map[string][]bindingcheck.Condition { return prober.State().Snapshot() },
			Observed: boot.Source.BindingStatus, Version: boot.Source.BindingEvents, Served: served, Timeout: statusCallTimeout(h.cfg.Election, h.leaseFence != nil), Fence: h.leaseFence, Warnings: h.status.warnings, Log: log, OnWrite: metrics.StatusWrite, OnUnserved: metrics.UnservedBindings,
		})
		if err != nil {
			return nil, fmt.Errorf("binding status: %w", err)
		}
	}
	prober = verification{
		Own: conn.Ref, Bindings: bindings, Platforms: chat.Narrow[platformReader](platforms), Conns: func() []Connection { return *connsServed.Load() },
		Snaps: boot.Reloader, Revision: func(ctx context.Context) string {
			return boot.Reloader.Revision(ctx) + "/" + strconv.FormatUint(connsChanged.Load(), 10)
		}, Secrets: boot.Secrets, Metrics: metrics, Log: log, Interval: cfg.probeEvery, Tick: cfg.probeTick, MinRevisionGap: cfg.probeGap,
		Status: statusW,
	}.newProber()
	if statusW != nil {
		refused.changed = statusW.notify
	}
	if statusW != nil {
		stops.add("binding-status", startLoop(runCtx, func(c context.Context) error { statusW.Run(c); return nil }))
	}
	stops.add("binding-verification", startLoop(runCtx, func(c context.Context) error { prober.Run(c); return nil }))

	interactions, err := parts.start(runCtx, mgr, ix, &stops)
	if err != nil {
		return nil, err
	}
	// A match of a botMessage trigger that starts a turn becomes a session through the manager's intake, the path a
	// mention takes. The threads of the keys are in the store; the permalink comes from what the platform learned when it
	// connected, since the start runs on the acknowledgement path and may call neither Slack nor the agent.
	starter, err := newTriggerStarter(triggerStarterConfig{
		Submit: mgr, Takes: mgr.Takes, Notice: sink.QueueNotice, Audit: auditor, Links: chat.Narrow[chat.Permalinker](platforms), Log: log,
	})
	if err != nil {
		return nil, err
	}
	// Each connection has an intake of its own (its table of triggers, the messages it has seen) over the parts that every connection
	// shares, behind a guard that drops a mention by the bot of another connection this broker runs (botGuard).
	newIntake := func(ref v1alpha1.ObjectRef) (chat.Sink, error) {
		intakeCfg, err := newIntakeConfig(intakeParts{
			conn: ref, snapshot: boot.Reloader.Current, submitter: mgr, interactions: interactions, log: log,
			notice: sink.QueueNotice, starter: starter, threads: st, runs: st, replies: st,
		})
		if err != nil {
			return nil, err
		}
		return botGuard{in: chat.NewIntake(intakeCfg), self: v1alpha1.NamespacedName(ref.Namespace, ref.Name), bots: table, log: log}, nil
	}
	ownSink, err := newIntake(conn.Ref)
	if err != nil {
		return nil, err
	}
	chatDone := make(chan error, 1)
	parts.traced("open-intake")
	host.run(ownRT, ownSink, func(_ *connRuntime, err error) { chatDone <- err })
	// The team connections: started by the supervisor, from the ones read with the own connection and then from the snapshot at each of its
	// looks. A team connection that cannot start yet (its Secret has not synced) starts when it can, and none fails this start.
	sup := &supervisor{
		host: host, log: log, every: cmp.Or(cfg.superviseEvery, defaultSuperviseEvery), snapshot: boot.Reloader.Current, secrets: boot.Secrets,
		own: conn, identify: cfg.identityOf, intake: newIntake, refusals: refused, conns: connsServed, changes: &connsChanged, gone: ob.Forget,
		removed: metrics.ConnectionRemoved, recover: mgr.RecoverConnection,
		done: make(chan struct{}), teams: map[string]*teamRuntime{}, seen: map[string]bool{}, lives: map[string]*connLife{}, said: map[string]string{},
	}
	supCtx, stopSup := context.WithCancel(runCtx)
	go sup.run(supCtx, chosen.Team)
	sd.add(stepIntake, func(c context.Context) { // the intakes stop before the interactions and the manager they feed
		stopSup()
		waitFor(c, sup.done) // the supervisor's calls are bounded once stopSup has ended its context, so this returns at once or soon
		host.haltAll(c)      // and a connection it adds after this, were the wait cut, is halted as it is added (runtimeTable.seal)
	})
	return chatDone, nil
}
