package agent

import (
	"cmp"
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
	// ConnectTimeout bounds a Pool's connect, which fetches the agent card. Default 30s.
	ConnectTimeout time.Duration
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
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 30 * time.Second
	}
	return c
}

// Agent talks to one agent through a Source. It is safe for concurrent use.
type Agent struct {
	source Source
	tool   ToolClassifier
	cfg    Config
}

// New returns an Agent that gets its connection from source and recognizes tool parts with
// tool, normally the backend dialect's ToolPart. A nil tool leaves data parts opaque.
func New(source Source, tool ToolClassifier, cfg Config) *Agent {
	return &Agent{source: source, tool: tool, cfg: cfg.withDefaults()}
}

// Send sends msg and delivers the reply to h as it streams, returning when the task ends
// or pauses for input (h has then got a FinalEvent) or when the send fails. Set msg.ID
// once and keep it: a message resent after ErrBusy must carry the same messageId. After
// ErrOutcomeUnknown, do not resend until FindTask has looked the turn up (see the error).
//
// A stream that drops, or stays silent for InactivityTimeout, is cancelled and reconciled:
// GetTask, then SubscribeToTask (spec 8.3), and no text is delivered twice. An error event
// mid-stream, over HTTP 200, is the send's outcome and comes back typed (see the Err values).
// The agent's card must advertise capabilities.streaming; the Pool checks it.
func (a *Agent) Send(ctx context.Context, msg *a2a.Message, h Handler) error {
	return a.send(ctx, msg, h, false)
}

// Answer is Send for a message that answers an interaction (an ask-user reply or a tool
// approval). It differs in one way: a -32603 (ErrAnswerRejected) leaves the task wedged, so
// Answer cancels it, the task the stream named or else msg.TaskID (R17). The caller tells
// the thread. If the cancel fails too, the error carries both.
func (a *Agent) Answer(ctx context.Context, msg *a2a.Message, h Handler) error {
	return a.send(ctx, msg, h, true)
}

func (a *Agent) send(ctx context.Context, msg *a2a.Message, h Handler, answer bool) error {
	conn, err := a.source(ctx)
	if err != nil {
		return fmt.Errorf("%w: connect to agent: %w", ErrNotSent, err)
	}
	asm := newAssembler(h, a.tool)
	err = a.read(ctx, asm, func(ctx context.Context) iter.Seq2[a2a.Event, error] {
		return conn.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	})
	switch {
	case err == nil:
		return nil
	case !dropped(ctx, err):
		err = sendError(ctx, err, answer)
		if errors.Is(err, ErrAnswerRejected) {
			err = a.cancelWedged(ctx, err, cmp.Or(asm.taskID, msg.TaskID))
		}
		return err
	case asm.taskID == "": // nothing to reconnect to
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	switch err = a.reconnect(ctx, asm, err); {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("send message: %w", err)
	}
	return &StreamLostError{TaskID: asm.taskID, ContextID: asm.contextID, Err: typed(err, false)}
}

// cancelWedged cancels the task a rejected answer left in WORKING, which refuses every
// message until then (R17). It runs detached from ctx, so the caller giving up does not
// leave the task wedged; UnaryTimeout bounds it. A failed cancel is joined to rejected.
func (a *Agent) cancelWedged(ctx context.Context, rejected error, id a2a.TaskID) error {
	if id == "" {
		return rejected
	}
	if _, err := a.CancelTask(context.WithoutCancel(ctx), id); err != nil {
		return errors.Join(rejected, err)
	}
	return rejected
}

// reconnect resumes the task, after err ended its stream, until the task settles. It gives
// up when MaxReconnects in a row change nothing (the opening snapshot of a resubscribe does
// not count), and on an error that is not a lost connection.
func (a *Agent) reconnect(ctx context.Context, asm *assembler, err error) error {
	for failures := 0; err != nil; {
		if failures++; !dropped(ctx, err) || failures > a.cfg.MaxReconnects {
			return err
		}
		select {
		case <-time.After(a.cfg.ReconnectDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
		conn, cerr := a.source(ctx) // resolves credentials again: the token may have rotated
		if cerr != nil {
			return fmt.Errorf("reconnect to agent: %w", cerr)
		}
		before := asm.changes
		err = a.resume(ctx, conn, asm)
		if asm.changes > before {
			failures = 0
		}
	}
	return nil
}

// resume reconciles a lost stream: GetTask, then, if the task still runs, SubscribeToTask.
// A snapshot replaces the text, so only what is new reaches the caller.
func (a *Agent) resume(ctx context.Context, conn Conn, asm *assembler) error {
	task, err := a.getTask(ctx, conn, asm.taskID)
	if err != nil {
		return err
	}
	asm.apply(task)
	if asm.final != nil {
		return nil
	}
	return a.read(ctx, asm, func(ctx context.Context) iter.Seq2[a2a.Event, error] {
		return conn.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: asm.taskID})
	})
}

var errInactive = errors.New("agent: no event within the inactivity timeout")

// read feeds a stream to asm until the task settles, cutting the stream after
// InactivityTimeout without an event. It returns the stream's error, errInactive when the
// watchdog cut it, or io.ErrUnexpectedEOF when it ended cleanly first.
func (a *Agent) read(ctx context.Context, asm *assembler, open func(context.Context) iter.Seq2[a2a.Event, error]) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(a.cfg.InactivityTimeout, cancel)
	defer watchdog.Stop()
	err := error(io.ErrUnexpectedEOF)
	for ev, streamErr := range open(sctx) {
		if streamErr != nil {
			err = streamErr
			break
		}
		watchdog.Reset(a.cfg.InactivityTimeout)
		asm.apply(ev)
		if asm.final != nil {
			return nil
		}
	}
	if ctx.Err() == nil && sctx.Err() != nil {
		err = errInactive
	}
	return err
}

// GetTask returns the task's current state. UnaryTimeout bounds it, resolving the
// connection included.
func (a *Agent) GetTask(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	defer cancel()
	conn, err := a.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to agent: %w", err)
	}
	return a.getTask(ctx, conn, id)
}

// CancelTask cancels the task and returns it as the agent left it. It backs the thread's
// stop control, and Answer calls it for a wedged task (R17). UnaryTimeout bounds it,
// resolving the connection included.
func (a *Agent) CancelTask(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	defer cancel()
	conn, err := a.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to agent: %w", err)
	}
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
