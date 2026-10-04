package broker

import (
	"context"
	"log/slog"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// intakeParts is what Run puts the chat Intake together from.
type intakeParts struct {
	conn         v1alpha1.ObjectRef
	snapshot     func() *config.Snapshot
	submitter    chat.Submitter
	interactions func(context.Context, chat.Interaction) error
	notice       func(context.Context, session.Key, session.Notice) error
	starter      *triggerStarter
	// threads keeps the thread of each trigger key, runs counts the starts each trigger's limits hold to, and
	// replies is the reply queue the Intake asks whether a reservation's turn was taken (store.ReplyQueue.HasReply):
	// all three are the one store.
	threads store.TriggerThreads
	runs    store.TriggerRuns
	replies store.ReplyQueue
	log     *slog.Logger
}

// newIntakeConfig is the Intake's configuration, in one place so that a test can see that every part of the
// botMessage triggers' path is connected: the thread store, the start (the rendered prompt, submitted as a turn),
// the check that closes the window between queueing a turn and marking its thread started, and the store of starts
// that the limits count in. Leaving any of them out compiles and runs: a keyed trigger without the thread store errors
// on every match, a trigger without the start investigates nothing, without the check a lost mark gives the key a
// second turn, and without the store of starts no limit holds.
func newIntakeConfig(p intakeParts) chat.IntakeConfig {
	return chat.IntakeConfig{
		Connection: p.conn, Snapshot: p.snapshot, Submitter: p.submitter, Logger: p.log,
		Interactions: p.interactions, Notice: p.notice,
		TriggerThreads: p.threads, TriggerRuns: p.runs, OnStart: p.starter.OnStart, TurnQueued: p.replies.HasReply,
	}
}
