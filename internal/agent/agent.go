package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Conn is the part of an A2A client that Agent uses. *a2aclient.Client implements it.
type Conn interface {
	SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error]
	SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error]
	GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error)
	CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error)
}

// Source returns the connection for one call, resolving credentials each time, so a rotated
// token takes effect on the next call. Agent asks again before every reconnect and keeps
// neither the connection nor any credential.
type Source func(ctx context.Context) (Conn, error)

// Config tunes an Agent. A zero field takes its default.
type Config struct {
	// InactivityTimeout is how long a stream may go without an event before Agent cuts it
	// and reconnects. Every event resets it. Set it longer than the agent's longest silent
	// tool call, since kagent sends nothing while a tool runs. Default 5 minutes.
	InactivityTimeout time.Duration
	// UnaryTimeout bounds GetTask and CancelTask, and each GetTask of a reconnect. Default 30s.
	UnaryTimeout time.Duration
	// MaxReconnects is how many reconnects in a row may fail, without a stream delivering an
	// event in between, before the send fails with ErrStreamLost. Default 5.
	MaxReconnects int
	// ReconnectDelay is the pause before each reconnect. Default 1s.
	ReconnectDelay time.Duration
}

func (c Config) withDefaults() Config {
	if c.InactivityTimeout <= 0 {
		c.InactivityTimeout = 5 * time.Minute
	}
	if c.UnaryTimeout <= 0 {
		c.UnaryTimeout = 30 * time.Second
	}
	if c.MaxReconnects <= 0 {
		c.MaxReconnects = 5
	}
	if c.ReconnectDelay <= 0 {
		c.ReconnectDelay = time.Second
	}
	return c
}

// Agent talks to one agent through a Source. It is safe for concurrent use.
type Agent struct {
	source Source
	cfg    Config
}

// New returns an Agent that gets its connection from source.
func New(source Source, cfg Config) *Agent { return &Agent{source: source, cfg: cfg.withDefaults()} }

// Send sends msg and delivers the reply to h as it streams, returning when the task ends
// or pauses for input (h has then got a FinalEvent) or when the send fails. Set msg.ID
// once and keep it: a resend after ErrBusy or a crash must carry the same messageId.
//
// A stream that drops, or stays silent for InactivityTimeout, is cancelled and reconciled:
// GetTask, then SubscribeToTask (spec 8.3), and no text is delivered twice. An error event
// mid-stream, over HTTP 200, is the send's outcome and comes back typed (see the Err values).
// The agent's card must advertise capabilities.streaming; the Pool checks it.
func (a *Agent) Send(ctx context.Context, msg *a2a.Message, h Handler) error {
	return a.send(ctx, msg, h, false)
}

// Answer is Send for a message that answers an interaction (an ask-user reply or a tool
// approval). It differs only in typing a -32603 as ErrAnswerRejected, after which the
// caller must CancelTask the task (R17).
func (a *Agent) Answer(ctx context.Context, msg *a2a.Message, h Handler) error {
	return a.send(ctx, msg, h, true)
}

func (a *Agent) send(ctx context.Context, msg *a2a.Message, h Handler, answer bool) error {
	conn, err := a.source(ctx)
	if err != nil {
		return fmt.Errorf("connect to agent: %w", err)
	}
	asm := newAssembler(h)
	progressed, err := a.read(ctx, asm, func(ctx context.Context) iter.Seq2[a2a.Event, error] {
		return conn.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	})
	for failures := 0; err != nil; {
		if progressed {
			failures = 0
		}
		failures++
		// Reconnect only after a dropped connection, and only once the agent has named the task.
		if failures > a.cfg.MaxReconnects || asm.taskID == "" || ctx.Err() != nil || isProtocol(err) {
			return sendError(ctx, err, asm.taskID, answer)
		}
		select {
		case <-time.After(a.cfg.ReconnectDelay):
		case <-ctx.Done():
			return sendError(ctx, ctx.Err(), asm.taskID, answer)
		}
		if conn, err = a.source(ctx); err != nil {
			return fmt.Errorf("reconnect to agent: %w", err)
		}
		progressed, err = a.resume(ctx, conn, asm)
	}
	return nil
}

// resume reconciles a lost stream: GetTask, then, if the task still runs, SubscribeToTask.
// A snapshot replaces the text, so only what is new reaches the caller.
func (a *Agent) resume(ctx context.Context, conn Conn, asm *assembler) (bool, error) {
	task, err := a.getTask(ctx, conn, asm.taskID)
	if err != nil {
		return false, err
	}
	asm.apply(task)
	if asm.final != nil {
		return true, nil
	}
	return a.read(ctx, asm, func(ctx context.Context) iter.Seq2[a2a.Event, error] {
		return conn.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: asm.taskID})
	})
}

var errInactive = errors.New("agent: no event within the inactivity timeout")

// read feeds a stream to asm until the task settles, cutting the stream after
// InactivityTimeout without an event. It returns the stream's error, errInactive when the
// watchdog cut it, or io.ErrUnexpectedEOF when it ended cleanly first. progressed says
// whether any event arrived.
func (a *Agent) read(ctx context.Context, asm *assembler, open func(context.Context) iter.Seq2[a2a.Event, error]) (progressed bool, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(a.cfg.InactivityTimeout, cancel)
	defer watchdog.Stop()
	err = io.ErrUnexpectedEOF
	for ev, streamErr := range open(sctx) {
		if streamErr != nil {
			err = streamErr
			break
		}
		progressed = true
		watchdog.Reset(a.cfg.InactivityTimeout)
		asm.apply(ev)
		if asm.final != nil {
			return true, nil
		}
	}
	if ctx.Err() == nil && sctx.Err() != nil {
		err = errInactive
	}
	return progressed, err
}

// GetTask returns the task's current state, within UnaryTimeout.
func (a *Agent) GetTask(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	conn, err := a.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to agent: %w", err)
	}
	return a.getTask(ctx, conn, id)
}

// CancelTask cancels the task, within UnaryTimeout, and returns it as the agent left it. It
// backs the thread's stop control and clears a wedged task after ErrAnswerRejected (R17).
func (a *Agent) CancelTask(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	conn, err := a.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to agent: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	defer cancel()
	task, err := conn.CancelTask(ctx, &a2a.CancelTaskRequest{ID: id})
	if err != nil {
		return nil, fmt.Errorf("cancel task %s: %w", id, typed(err, false))
	}
	return task, nil
}

func (a *Agent) getTask(ctx context.Context, conn Conn, id a2a.TaskID) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	defer cancel()
	task, err := conn.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", id, typed(err, false))
	}
	return task, nil
}
