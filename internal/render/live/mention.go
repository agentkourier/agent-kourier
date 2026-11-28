package live

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// MentionTarget identifies a turn and its Binding's mention settings when notifying declared people.
type MentionTarget struct {
	Source      string
	Binding     string
	Connection  string
	Channel     string
	Thread      string
	Mentions    []v1alpha1.ChatMention
	MaxMentions int
}

// mentionLock serializes mention checks and enqueues for one turn Source while any writer is active.
type mentionLock struct {
	mu   sync.Mutex
	refs int
}

func (r *Renderer) lockMentions(source string) func() {
	r.mentionMu.Lock()
	l := r.mentionLocks[source]
	if l == nil {
		l = &mentionLock{}
		r.mentionLocks[source] = l
	}
	l.refs++
	r.mentionMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		r.mentionMu.Lock()
		l.refs--
		if l.refs == 0 && r.mentionLocks[source] == l {
			delete(r.mentionLocks, source)
		}
		r.mentionMu.Unlock()
	}
}

// NotifyQuestionMentions queues mention messages for declared people named in a delivered pause prompt's questions,
// sharing the turn's cap, deduplication, and serialization with the turn's reply mentions.
func (r *Renderer) NotifyQuestionMentions(ctx context.Context, target MentionTarget, questions []string) {
	r.notifyMentions(ctx, target, questions, render.QuestionMentionMessage)
}

// notifyMentions queues the messages that notify the people the turn's final text names, one for each in a row of its own and
// after the reply, so that each is a new message wherever the reply was streamed or updated: a mention beside a body notifies nobody
// (chat.Mention). A turn that failed, was stopped or was rejected notifies nobody, as one cut short does: a person who stops a
// steered agent has decided against what it was saying. A pause counts: what the agent wrote before it may be for the person it
// names, and a delivered question prompt notifies through NotifyQuestionMentions under the same turn source and cap. A person
// with a row is not mentioned again, and counts towards the cap: rendered again a turn posts and records nothing, and a task
// that paused and ran on under the same turn gets rows only for whoever its later text names first. Only the outbox's delivery
// is retried: a crash before this runs loses the mention, which is a convenience, and never the paging path, and a crash after
// a row is queued, or a timeout, can leave it with no audit row, since a row found is not recorded again. A failure is logged,
// and the reply stands.
func (u *Run) notifyMentions(ctx context.Context) {
	u.mu.Lock()
	endedBadly := u.endedBadly
	u.mu.Unlock()
	if endedBadly || len(u.turn.Mentions) == 0 {
		return
	}
	u.owner.notifyMentions(ctx, MentionTarget{
		Source:      u.turn.Source,
		Binding:     u.turn.Binding,
		Connection:  u.turn.Connection,
		Channel:     u.turn.Channel,
		Thread:      u.turn.Thread,
		Mentions:    u.turn.Mentions,
		MaxMentions: u.turn.MaxMentions,
	}, []string{u.snapshot().body}, render.MentionMessage)
}

func (r *Renderer) notifyMentions(ctx context.Context, target MentionTarget, texts []string, format func(v1alpha1.ChatMention) chat.Message) {
	if target.Source == "" || len(target.Mentions) == 0 || len(texts) == 0 {
		return
	}
	unlock := r.lockMentions(target.Source)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.WriteTimeout)
	defer cancel()
	already, err := r.mentioned(ctx, target.Source, target.Mentions)
	if err != nil {
		r.cfg.Logger.WarnContext(ctx, "live: the mentions of a turn were not queued", "channel", target.Channel, "err", err)
		return
	}
	scan := render.MentionsIn(texts, target.Mentions, target.MaxMentions, already...)
	for range scan.Undeclared {
		telemetry.Count(ctx, "agentkourier_mentions_total", target.Binding, "undeclared")
	}
	for range scan.Capped {
		telemetry.Count(ctx, "agentkourier_mentions_total", target.Binding, "capped")
	}
	for _, m := range scan.Made {
		post := outbox.Post{Source: outbox.MentionSource(target.Source, m.User), Role: store.OutboxNotice, Connection: target.Connection, Channel: target.Channel, Thread: target.Thread, Out: chat.Say(format(m))}
		if _, err := r.cfg.Sender.Enqueue(ctx, post); err != nil {
			r.cfg.Logger.WarnContext(ctx, "live: a mention of a turn was not queued", "outbox_id", outbox.ID(post.Source, post.Role), "channel", target.Channel, "err", err)
			continue
		}
		telemetry.Count(ctx, "agentkourier_mentions_total", target.Binding, "made")
		if r.cfg.Audit == nil {
			continue
		}
		rec := audit.Mention{Turn: target.Source, Channel: target.Channel, Thread: target.Thread, Name: m.Name, User: m.User}
		if err := r.cfg.Audit.Mention(ctx, audit.Actor{Binding: target.Binding}, rec); err != nil {
			r.cfg.Logger.WarnContext(ctx, "live: a mention was not audited", "channel", target.Channel, "err", err)
		}
	}
}

// mentioned is the declared users the turn has a row for: pending, delivered or abandoned, each of which has had its one try.
func (r *Renderer) mentioned(ctx context.Context, source string, declared []v1alpha1.ChatMention) ([]string, error) {
	var users []string
	for _, m := range declared {
		if slices.Contains(users, m.User) {
			continue
		}
		_, err := r.cfg.Store.GetOutbox(ctx, outbox.ID(outbox.MentionSource(source, m.User), store.OutboxNotice))
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, fmt.Errorf("read the mentions of the turn: %w", err)
		default:
			users = append(users, m.User)
		}
	}
	return users, nil
}
