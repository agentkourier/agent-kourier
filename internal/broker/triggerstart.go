package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// triggerAuditor is what the starter needs of the audit log: *audit.Writer.
type triggerAuditor interface {
	TriggerRefused(ctx context.Context, a audit.Actor, r audit.TriggerRefusal) error
}

// triggerStarter turns a botMessage trigger's decision to start into an investigation (spec 8.2): it renders the
// first prompt from the trigger's template and the matched message, and gives it to the session manager's own
// intake, the durable path a mention takes, anchored to the alert's thread. It is the Intake's IntakeConfig.OnStart.
//
// It runs on the platform's acknowledgement path, which has seconds, so it does local work only: a render, a few local
// writes, and a wake. It calls no agent and no Slack API; the permalink comes from what the platform cached when it
// connected.
type triggerStarter struct {
	// conn is the ChatConnection as it appears in keys and message IDs: namespace/name.
	conn   string
	submit chat.Submitter
	// notice queues a note in a thread (live.Sink.QueueNotice); its error means the note was not written.
	notice func(ctx context.Context, key session.Key, n session.Notice) error
	audit  triggerAuditor
	// link builds the permalink of a message, and says false if the platform cannot (chat.Permalinker). Nil
	// is the same: the view's Link is then empty.
	link func(chat.Ref) (string, bool)
	log  *slog.Logger
}

// permalinks returns the permalink builder of p, or nil if the platform makes none.
func permalinks(p chat.Platform) func(chat.Ref) (string, bool) {
	if l, ok := p.(chat.Permalinker); ok {
		return l.Permalink
	}
	return nil
}

// OnStart implements IntakeConfig.OnStart.
//
// The first turn's text is the rendered prompt (render.Prompt): the template's output, then the whole message in the
// untrusted block. The turn names no user: nobody wrote it, so it is filed under the session manager's system
// user (session.SystemUser), which sends no attribution and audits no person, rather than a person who did not write
// it. A person's reply in the thread continues the session without a mention, as a reply in any thread courier
// owns does.
//
// The session keeps the alert's name (the match's "alertname" entry, if it has the shape of a name: feedback.AlertName),
// so that a vote on any message of the thread, cast long after the start, is stored and counted under it (spec 8.7).
//
// A prompt that cannot be rendered, whether a value failed its check (render.RefusedError) or the template failed
// to run, starts no turn. The thread gets a fixed note, the audit log an entry, and the counter a count (refuse),
// and the error wraps chat.ErrNotStarted, which the Intake treats as handled.
//
// An error that does not wrap it, from the manager or from queueing the note, means the start was not taken: the
// platform delivers the event again, and the redelivery asks again, which is idempotent by the alert's message ID.
func (s *triggerStarter) OnStart(ctx context.Context, m chat.TriggerMatch) error {
	at := m.Message.At
	view := v1alpha1.PromptView{
		Channel: at.Channel, State: m.Fields["state"], Key: m.Fields["key"], Source: cmp.Or(m.Preset, "botMessage"), Fields: m.Fields,
	}
	if s.link != nil {
		if link, ok := s.link(at); ok {
			view.Link = link
		} else {
			s.log.DebugContext(ctx, "chat: the platform made no permalink for an alert; the prompt's link is empty", "channel", at.Channel, "message", at.ID)
		}
	}
	prompt, err := render.NewPrompt("promptTemplate", m.PromptTemplate) // parsed at load too; this is the same parse
	var text string
	if err == nil {
		text, err = prompt.Render(view, render.MessageOf(m.Message))
	}
	if err != nil {
		return s.refuse(ctx, m, err)
	}
	id := m.MessageID
	res, err := s.submit.Submit(ctx, session.Inbound{
		Key:       session.Key{Connection: s.conn, Channel: at.Channel, Thread: at.ID},
		Binding:   m.Binding,
		MessageID: id, MessageRef: at.ID, Text: text, Create: true, Alert: feedback.AlertName(m.Fields["alertname"]),
	})
	switch {
	case err != nil:
		return fmt.Errorf("start the investigation of %s: %w", id, err)
	case res.Disposition != session.Created && res.Disposition != session.Accepted:
		return fmt.Errorf("start the investigation of %s: the session manager did not take it (%s)", id, res.Disposition)
	}
	return nil
}

// refuse is the visible half of a refused start: the note in the alert's thread, the audit entry and the count. The
// note is queued first, and its failure is returned as it is, before anything is audited or counted, so that the
// redelivery that follows does those once. The audit log failing is logged and counted by the Writer and does not
// undo the refusal. Nothing here names the value: the ref and reason are from closed sets, and only the value's
// length is kept.
func (s *triggerStarter) refuse(ctx context.Context, m chat.TriggerMatch, cause error) error {
	at := m.Message.At
	ref, reason, length, detail := "template", "render_error", 0, "the prompt template could not be rendered."
	var refused *render.RefusedError
	if errors.As(cause, &refused) {
		ref, reason, length, detail = refused.Ref, refused.Reason.Code(), refused.Len, "."+refused.Ref+" failed the prompt check."
	}
	id := m.MessageID
	key := session.Key{Connection: s.conn, Channel: at.Channel, Thread: at.ID}
	n := session.Notice{ID: id + "/" + string(session.NoticeTriggerRefused), Kind: session.NoticeTriggerRefused, MessageRef: at.ID, Detail: detail}
	if err := s.notice(ctx, key, n); err != nil {
		return fmt.Errorf("note the refused start of %s: %w", id, err)
	}
	if err := s.audit.TriggerRefused(ctx, audit.Actor{Binding: m.Binding}, audit.TriggerRefusal{MessageID: id, Trigger: m.Index, Ref: ref, Reason: reason, Len: length}); err != nil {
		s.log.WarnContext(ctx, "chat: the refusal of a trigger's start was not audited", "binding", m.Binding, "trigger", m.Index, "message", at.ID, "error", err)
	}
	telemetry.Count(ctx, "courier_trigger_refused_total", m.Binding, strconv.Itoa(m.Index), ref, reason)
	// The render error of a template that failed to run is logged by its text, which holds names of the template and
	// Go's own words. A refusal's holds neither the value nor its text, only the ref, reason and length.
	if refused == nil {
		s.log.WarnContext(ctx, "chat: a trigger's prompt could not be rendered, so no turn started", "binding", m.Binding, "trigger", m.Index, "channel", at.Channel, "message", at.ID, "error", cause)
	} else {
		s.log.WarnContext(ctx, "chat: a trigger's prompt was refused, so no turn started", "binding", m.Binding, "trigger", m.Index, "channel", at.Channel, "message", at.ID, "ref", ref, "reason", reason, "len", length)
	}
	return fmt.Errorf("trigger %d of %s for %s: %w", m.Index, m.Binding, id, chat.ErrNotStarted)
}
