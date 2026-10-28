package broker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
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

// start starts the leader half on conn, the ChatConnection and credentials as they are now (a standby that leads hours
// after it started must not use the tokens it saw at start: a takeover is a restart to the new leader). Its parts run on
// runCtx (the broker's own, which only the shutdown ends; ctx is Run's, the signal, which it only logs with). It returns the channel that says how the chat connection's run ended. An error
// from a step is returned as it is; whatever started before it has registered its stop. It must not be called twice, or
// once a shutdown has begun (Run's startGate sees to both).
func (h *leaderHalf) start(ctx, runCtx context.Context, conn Connection) (<-chan error, error) {
	cfg, log, sd, metrics, boot, st := h.cfg, h.log, h.sd, h.metrics, h.boot, h.st
	connName := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	var auditLog store.Audit = st
	if cfg.auditStore != nil {
		auditLog = cfg.auditStore(st)
	}
	auditor, err := audit.New(auditLog, audit.Options{Logger: log, OnFailure: metrics.AuditFailure})
	if err != nil {
		return nil, err
	}
	// The chat platform, built for the connection's ChatConnection.platform: it posts for the outbox, and a token revoked
	// mid-run, which only a post sees, ends the run with ErrSlackAuth (WatchAuth, WithAuthFailure).
	// agentkourier_chat_connected says 0 from here, and 1 while the connection is up. /readyz does not hold it: see NewHandler.
	metrics.ChatConnected(connName, false)
	platform, err := h.openPlatform(conn, func(up bool) { metrics.ChatConnected(connName, up) })
	if err != nil {
		return nil, err
	}

	// The Bindings take the platform's own acknowledgment reaction, so the platform is built first.
	bindings, err := NewBindings(boot.Reloader, boot.Secrets, auditor, Options{
		OnRejected: metrics.CredentialRejected, AckReaction: platform.Capabilities().AckReaction, fence: h.leaseFence})
	if err != nil {
		platform.closeIdle()
		return nil, err
	}
	sd.add(stepPool, func(context.Context) { bindings.Close() })

	chatCtx, report, stopChat := WithAuthFailure(runCtx)
	ob, err := outbox.New(outbox.Config{
		Store: st, Platform: WatchAuth(platform, report), Logger: log, OnAbandon: metrics.OutboxAbandoned,
	})
	if err != nil {
		return nil, err
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
		// calls opened after an earlier close would be one that nothing closes.
		platform.closeIdle()
	})

	// The renderer posts a turn's first message through the outbox and edits or streams it through the
	// chat platform (the Slack adapter over the client); a notice is only queued on the outbox, off Slack's
	// acknowledgement path.
	renderer, err := live.New(live.Config{Sender: ob, Platform: platform, Store: st, Logger: log})
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
	parts := interactionDeps{st: st, ob: ob, platform: platform, auditor: auditor, bindings: bindings, metrics: metrics, log: log, trace: cfg.startTrace}
	ix, err := parts.build(sink)
	if err != nil {
		return nil, err
	}
	// A mentioned turn of a mention-only Binding carries the replies since the previous turn, which are read through the
	// platform as it is built here (fenced with an election: the read is not a write, and the fence forwards it).
	threads, err := newThreadContext(threadContextConfig{Platform: platform.Platform, Replies: st, Logger: log})
	if err != nil {
		return nil, err
	}
	// A turn's acknowledgment removal is queued and its reply settled at once, so that retention does not queue it again.
	var unack func(context.Context, session.Key, string, session.Notice) error
	if settler, ok := st.(store.AcknowledgmentLister); ok {
		unack = unacknowledge(sink.QueueNotice, settler.SettleAcknowledgmentReplies)
	}
	mgr, err := session.New(session.Config{Context: runCtx, Store: st, Questions: st, Bindings: bindings, Users: users{platform}, Sink: ix.tap, TextReply: ix.replies.Decide, Logger: log, Audit: auditor, OnTurnEnd: metrics.TurnOutcome, ThreadContext: threads.Section, AnswerWatcher: ix.reopened, Unacknowledge: unack})
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
	retain, err := newRetention(retentionConfig{Store: st, Interval: cfg.retentionEvery, AuditRetention: cfg.AuditRetention, ThreadTTLs: bindings.ThreadTTLs,
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
	interactions, err := parts.start(runCtx, mgr, ix, &stops)
	if err != nil {
		return nil, err
	}

	// A match of a botMessage trigger that starts a turn becomes a session through the manager's intake, the path a
	// mention takes. The threads of the keys are in the store; the permalink comes from what the platform learned when it
	// connected, since the start runs on the acknowledgement path and may call neither Slack nor the agent.
	starter, err := newTriggerStarter(triggerStarterConfig{
		Conn: connName, Submit: mgr, Notice: sink.QueueNotice, Audit: auditor, Link: permalinks(platform.Platform), Log: log,
	})
	if err != nil {
		return nil, err
	}
	intakeCfg, err := newIntakeConfig(intakeParts{
		conn: conn.Ref, snapshot: boot.Reloader.Current, submitter: mgr, interactions: interactions, log: log,
		notice: sink.QueueNotice, starter: starter, threads: st, runs: st, replies: st,
	})
	if err != nil {
		return nil, err
	}
	intake := chat.NewIntake(intakeCfg)
	chatDone, chatStopped := make(chan error, 1), make(chan struct{})
	parts.traced("open-intake")
	go func() {
		chatDone <- RunPlatform(chatCtx, platform, intake, RunPolicy{Logger: log})
		close(chatStopped)
	}()
	sd.add(stepIntake, func(c context.Context) { // the intake stops before the interactions and the manager it feeds
		stopChat()
		waitFor(c, chatStopped)
	})
	return chatDone, nil
}
