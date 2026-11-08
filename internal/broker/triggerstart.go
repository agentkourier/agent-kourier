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
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// botMessageSource and personMessageSource are the sources a trigger start reports: the prompt view's Source and the
// session's origin source. A start reports the type of the trigger that matched. Feedback attributes an alert by name to
// botMessage alone, with an alertname.
const (
	botMessageSource    = string(v1alpha1.TriggerBotMessage)
	personMessageSource = store.OriginPersonMessage
)

// botMentionSource is the source a bot's mention reports. It is no alert, so feedback attributes no alert to it.
const botMentionSource = store.OriginBotMention

// triggerAuditor is what the starter needs of the audit log: *audit.Writer.
type triggerAuditor interface {
	TriggerRefused(ctx context.Context, a audit.Actor, r audit.TriggerRefusal) error
}

// triggerStarterConfig is what a triggerStarter is made from. Every field is required: newTriggerStarter refuses a config
// without one and names it. Link is the one that may legitimately do nothing: a platform that makes no links says false.
type triggerStarterConfig struct {
	Submit chat.Submitter
	// Takes says whether a turn would be queued, writing nothing (session.Manager.Takes).
	Takes func(ctx context.Context, in session.Inbound) (bool, error)
	// Notice queues a note in a thread (live.Sink.QueueNotice); its error means the note was not written.
	Notice func(ctx context.Context, key session.Key, n session.Notice) error
	Audit  triggerAuditor
	// Links gives the platform of a message's connection, which builds the permalink of a message (chat.Permalinker), and says
	// false if the platform cannot: the view's Link is then empty, as it is for a connection that is not running.
	Links chat.Resolver[chat.Permalinker]
	Log   *slog.Logger
}

// triggerStarter turns a botMessage or personMessage trigger's decision to start into an investigation (spec 8.2): it
// renders the first prompt from the trigger's template and the matched message, and gives it to the session manager's own
// intake, the durable path a mention takes, anchored to the matched message's thread. It is the Intake's IntakeConfig.OnStart.
//
// It runs on the platform's acknowledgement path, which has seconds, so it does local work only: a render, a few local
// writes, and a wake. It calls no agent and no Slack API; the permalink comes from what the platform cached when it
// connected.
type triggerStarter struct {
	submit chat.Submitter
	takes  func(ctx context.Context, in session.Inbound) (bool, error)
	notice func(ctx context.Context, key session.Key, n session.Notice) error
	audit  triggerAuditor
	links  chat.Resolver[chat.Permalinker]
	log    *slog.Logger
}

// newTriggerStarter returns a triggerStarter over cfg, or an error that names the field that cfg leaves out.
func newTriggerStarter(cfg triggerStarterConfig) (*triggerStarter, error) {
	switch {
	case cfg.Submit == nil:
		return nil, errors.New("broker: Submit is required for the trigger start: it is how a turn is queued")
	case cfg.Takes == nil:
		return nil, errors.New("broker: Takes is required for the trigger start: a bot's mention asks it before the limits")
	case cfg.Notice == nil:
		return nil, errors.New("broker: Notice is required for the trigger start: it is how a refused start is told to the thread")
	case cfg.Audit == nil:
		return nil, errors.New("broker: Audit is required for the trigger start: a refused start is recorded")
	case cfg.Links == nil:
		return nil, errors.New("broker: Links is required for the trigger start (the platforms' Permalink)")
	case cfg.Log == nil:
		return nil, errors.New("broker: Log is required for the trigger start")
	}
	return &triggerStarter{submit: cfg.Submit, takes: cfg.Takes, notice: cfg.Notice, audit: cfg.Audit, links: cfg.Links, log: cfg.Log}, nil
}

// OnStart implements IntakeConfig.OnStart.
//
// The first turn's text is the rendered prompt (render.Prompt): the template's output, then the whole message in the
// untrusted block. A bot's message and a bot's mention name no user: nobody wrote the turn, so it is filed under the
// session manager's system user (session.SystemUser), which sends no attribution and audits no person, rather than a
// person who did not write it. A person's message (m.Person) is that person's turn: it names its author, so the session
// manager attributes it, audits the person and shows the answer to them as it does for any turn a person writes, and a
// refusal is audited as theirs. A person's reply in an alert's thread continues the session without a mention, as a
// reply in any thread Agent Kourier owns does; in the thread of a person's message, as in one a bot's mention began, only
// a mentioned reply does (session.Manager, by the origin).
//
// The session keeps the trigger source and its bounded alertname subject, so that a vote on any message of the thread,
// cast long after the start, can be attributed at the feedback boundary (spec 8.7).
//
// A prompt that cannot be rendered, whether a value failed its check (render.RefusedError) or the template failed
// to run, starts no turn. The thread gets a fixed note, the audit log an entry, and the counter a count (refuse),
// and the error wraps chat.ErrNotStarted, which the Intake treats as handled.
//
// An error that does not wrap it, from the manager or from queueing the note, means the start was not taken: the
// platform delivers the event again, and the redelivery asks again, which is idempotent by the alert's message ID.
//
// A bot's mention (m.Mention) is a turn of the thread the mention is in, whose session may exist already, and it carries that
// thread. The Intake asked already whether its session takes a turn (chat.IntakeConfig.TakesBotMention); one that stopped since
// is ErrNotStarted with no note: the bot asked, and a note answers no one.
func (s *triggerStarter) OnStart(ctx context.Context, m chat.TriggerMatch) error {
	at := m.Message.At
	if at.Conn == "" { // an adapter names the connection of every message it forwards; one that does not could file the session under none
		s.log.ErrorContext(ctx, "chat: a bot message names no connection, so it started nothing", "binding", m.Binding, "channel", at.Channel, "message", at.ID)
		return fmt.Errorf("bot message %s names no connection: %w", m.MessageID, chat.ErrNotStarted)
	}
	source := botMessageSource
	switch {
	case m.Mention:
		source = botMentionSource
	case m.Person:
		source = personMessageSource
	}
	view := v1alpha1.PromptView{
		Channel: at.Channel, State: m.Fields["state"], Key: m.Fields["key"], Source: source, Fields: m.Fields,
	}
	if link, ok := s.permalink(at); ok {
		view.Link = link
	} else {
		s.log.DebugContext(ctx, "chat: the platform made no permalink for an alert; the prompt's link is empty", "channel", at.Channel, "message", at.ID)
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
		Key:       s.keyOf(m),
		Binding:   m.Binding,
		MessageID: id, MessageRef: at.ID, UserID: authorOf(m), Text: text, Create: true, WholeThread: m.Mention,
		Origin: store.Origin{Source: source, Subject: feedback.AlertName(m.Fields["alertname"])},
	})
	switch {
	case err != nil:
		return fmt.Errorf("start the investigation of %s: %w", id, err)
	case m.Mention && !res.Admission.Queues():
		s.log.InfoContext(ctx, "chat: a bot's mention took no turn; the thread cannot be continued", "binding", m.Binding, "channel", at.Channel, "message", at.ID, "disposition", res.Disposition)
		return fmt.Errorf("bot mention of %s: %w", id, chat.ErrNotStarted)
	case res.Disposition != session.Created && res.Disposition != session.Accepted:
		return fmt.Errorf("start the investigation of %s: the session manager did not take it (%s)", id, res.Disposition)
	}
	return nil
}

// authorOf is who wrote the turn a match starts: the person of a person's message, and nobody for a bot's message or mention,
// whatever user ID the sender's message carries.
func authorOf(m chat.TriggerMatch) string {
	if m.Person {
		return m.Message.User
	}
	return ""
}

// permalink is the link to the message at, made by the platform of the message's connection.
func (s *triggerStarter) permalink(at chat.Ref) (string, bool) {
	p, err := s.links.Platform(at.Conn)
	if err != nil {
		return "", false
	}
	return p.Permalink(at)
}

// TakesBotMention says whether the session of a bot's mention would take it as a turn (chat.IntakeConfig.TakesBotMention).
func (s *triggerStarter) TakesBotMention(ctx context.Context, m chat.TriggerMatch) (bool, error) {
	return s.takes(ctx, session.Inbound{Key: s.keyOf(m), Binding: m.Binding, Create: true, WholeThread: true})
}

// keyOf is the session a match is a turn of: an alert's own thread, or the thread a bot's mention is in.
func (s *triggerStarter) keyOf(m chat.TriggerMatch) session.Key {
	thread := m.Message.At.ID
	if m.Mention {
		thread = cmp.Or(m.Message.At.Thread, thread)
	}
	return session.Key{Connection: m.Message.At.Conn, Channel: m.Message.At.Channel, Thread: thread}
}

// Limited records that a limit stopped a bot's mention or a person's message (chat.IntakeConfig.OnLimited): a person's under that
// person's user ID, so that a member who spends a trigger's limits for everyone can be found, and a bot's under nobody. It names the
// message by ID and never by what it said. Its failure is logged and counted by the Writer, and undoes nothing.
func (s *triggerStarter) Limited(ctx context.Context, m chat.TriggerMatch, limit store.TriggerLimit) {
	var ref string
	switch {
	case m.Person:
		ref = personMessageSource
	case m.Mention:
		ref = botMentionSource
	default: // an alert's stopped start is counted, not audited (chat.IntakeConfig.OnLimited)
		return
	}
	if err := s.audit.TriggerRefused(ctx, audit.Actor{Binding: m.Binding, User: authorOf(m)}, audit.TriggerRefusal{MessageID: m.MessageID, Trigger: m.Index, Ref: ref, Reason: "limit_" + string(limit)}); err != nil {
		s.log.WarnContext(ctx, "chat: a start that a limit stopped was not audited", "binding", m.Binding, "source", ref, "channel", m.Message.At.Channel, "message", m.Message.At.ID, "error", err)
	}
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
	key := s.keyOf(m)
	n := session.Notice{ID: id + "/" + string(session.NoticeTriggerRefused), Kind: session.NoticeTriggerRefused, MessageRef: at.ID, Detail: detail}
	if err := s.notice(ctx, key, n); err != nil {
		return fmt.Errorf("note the refused start of %s: %w", id, err)
	}
	if err := s.audit.TriggerRefused(ctx, audit.Actor{Binding: m.Binding, User: authorOf(m)}, audit.TriggerRefusal{MessageID: id, Trigger: m.Index, Ref: ref, Reason: reason, Len: length}); err != nil {
		s.log.WarnContext(ctx, "chat: the refusal of a trigger's start was not audited", "binding", m.Binding, "trigger", m.Index, "message", at.ID, "error", err)
	}
	telemetry.Count(ctx, "agentkourier_trigger_refused_total", m.Binding, strconv.Itoa(m.Index), ref, reason)
	// The render error of a template that failed to run is logged by its text, which holds names of the template and
	// Go's own words. A refusal's holds neither the value nor its text, only the ref, reason and length.
	if refused == nil {
		s.log.WarnContext(ctx, "chat: a trigger's prompt could not be rendered, so no turn started", "binding", m.Binding, "trigger", m.Index, "channel", at.Channel, "message", at.ID, "error", cause)
	} else {
		s.log.WarnContext(ctx, "chat: a trigger's prompt was refused, so no turn started", "binding", m.Binding, "trigger", m.Index, "channel", at.Channel, "message", at.ID, "ref", ref, "reason", reason, "len", length)
	}
	return fmt.Errorf("trigger %d of %s for %s: %w", m.Index, m.Binding, id, chat.ErrNotStarted)
}
