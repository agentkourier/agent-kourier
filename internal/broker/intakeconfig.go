package broker

import (
	"context"
	"errors"
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
// that the limits count in. chat.IntakeConfig leaves most of them optional, for the adapters and tests that have no
// such path, and leaving any of them out compiles and runs: a keyed trigger without the thread store errors on every
// match, a trigger without the start investigates nothing, without the check a lost mark gives the key a second turn,
// without the store of starts no limit holds, and without the interactions' handler every press of a button is dropped,
// acknowledged. The broker's own wiring has none of those choices, so this refuses a part that is left out, and names it.
func newIntakeConfig(p intakeParts) (chat.IntakeConfig, error) {
	switch {
	case p.conn.Name == "":
		return chat.IntakeConfig{}, errors.New("broker: conn is required for the intake")
	case p.snapshot == nil:
		return chat.IntakeConfig{}, errors.New("broker: snapshot is required for the intake: it is the routing table")
	case p.submitter == nil:
		return chat.IntakeConfig{}, errors.New("broker: submitter is required for the intake: it is how a message becomes a turn")
	case p.interactions == nil:
		return chat.IntakeConfig{}, errors.New("broker: interactions is required for the intake: it takes the presses of buttons")
	case p.notice == nil:
		return chat.IntakeConfig{}, errors.New("broker: notice is required for the intake: it tells a thread that Agent Kourier cannot continue it")
	case p.starter == nil:
		return chat.IntakeConfig{}, errors.New("broker: starter is required for the intake: it starts the turn of a trigger")
	case p.threads == nil:
		return chat.IntakeConfig{}, errors.New("broker: threads is required for the intake: it keeps the thread of each trigger key")
	case p.runs == nil:
		return chat.IntakeConfig{}, errors.New("broker: runs is required for the intake: without it no trigger limit holds")
	case p.replies == nil:
		return chat.IntakeConfig{}, errors.New("broker: replies is required for the intake: it says whether a reservation's turn was taken")
	case p.log == nil:
		return chat.IntakeConfig{}, errors.New("broker: log is required for the intake")
	}
	return chat.IntakeConfig{
		Connection: p.conn, Snapshot: p.snapshot, Submitter: p.submitter, Logger: p.log,
		Interactions: p.interactions, Notice: p.notice,
		TriggerThreads: p.threads, TriggerRuns: p.runs, OnStart: p.starter.OnStart, TurnQueued: p.replies.HasReply,
	}, nil
}
