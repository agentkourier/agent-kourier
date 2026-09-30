package agent

import (
	"context"
	"fmt"
	"io"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Conn is the part of an A2A client that Agent uses. *a2aclient.Client implements it.
type Conn interface {
	SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error]
}

// Source returns the connection to use for one call. It resolves credentials each time it
// runs, so a rotated token takes effect on the next call; Agent keeps neither the
// connection nor any credential.
type Source func(ctx context.Context) (Conn, error)

// Agent talks to one agent through a Source. It is safe for concurrent use.
type Agent struct {
	source Source
}

// New returns an Agent that gets its connection from source.
func New(source Source) *Agent { return &Agent{source: source} }

// Send sends msg and delivers the reply to h as it streams, returning when the task ends
// or pauses for input (h has then got a FinalEvent) or when the send fails. Set msg.ID
// once and keep it: a resend after ErrBusy or a crash must carry the same messageId.
//
// The agent requires the card's capabilities.streaming (the Pool checks it). An error
// event mid-stream, after the task began over HTTP 200, is the send's outcome and comes
// back typed: see ErrBusy, ErrForbidden, and ErrStreamLost.
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
	if err := drain(conn.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}), asm); err != nil {
		return sendError(ctx, err, asm.taskID, answer)
	}
	return nil
}

// drain feeds a stream to asm until the task settles. It returns the stream's error, or
// io.ErrUnexpectedEOF when the stream ends cleanly before the task settled.
func drain(seq iter.Seq2[a2a.Event, error], asm *assembler) error {
	for ev, err := range seq {
		if err != nil {
			return err
		}
		asm.apply(ev)
		if asm.final != nil {
			return nil
		}
	}
	return io.ErrUnexpectedEOF
}
