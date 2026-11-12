package live

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// notifyMentions queues the messages that notify the people the turn's final text names, one for each in a row of its own and
// after the reply, so that each is a new message wherever the reply was streamed or updated: a mention beside a body notifies nobody
// (chat.Mention). A turn that failed, was stopped or was rejected notifies nobody, as one cut short does: a person who stops a
// steered agent has decided against what it was saying. A pause counts: what the agent wrote before it may be for the person it
// names. The question itself is not read: it is the task's status message, which the interaction intake posts as a prompt, and is
// not the turn's text. A person with a row is not mentioned again, and counts towards the cap: rendered again a turn posts and
// records nothing, and a task that paused and ran on under the same turn gets rows only for whoever its later text names first.
// Only the outbox's delivery is retried: a crash before this runs loses the mention, which is a convenience, and never the paging
// path, and a crash after a row is queued, or a timeout, can leave it with no audit row, since a row found is not recorded again.
// A failure is logged, and the reply stands.
func (u *Run) notifyMentions(ctx context.Context) {
	u.mu.Lock()
	endedBadly := u.endedBadly
	u.mu.Unlock()
	if endedBadly || len(u.turn.Mentions) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.WriteTimeout)
	defer cancel()
	already, err := u.mentioned(ctx)
	if err != nil {
		u.cfg.Logger.WarnContext(ctx, "live: the mentions of a turn were not queued", "channel", u.turn.Channel, "err", err)
		return
	}
	scan := render.Mentions(u.snapshot().body, u.turn.Mentions, u.turn.MaxMentions, already...)
	for range scan.Undeclared {
		telemetry.Count(ctx, "agentkourier_mentions_total", u.turn.Binding, "undeclared")
	}
	for range scan.Capped {
		telemetry.Count(ctx, "agentkourier_mentions_total", u.turn.Binding, "capped")
	}
	for _, m := range scan.Made {
		post := outbox.Post{Source: outbox.MentionSource(u.turn.Source, m.User), Role: store.OutboxNotice, Connection: u.turn.Connection, Channel: u.turn.Channel, Thread: u.turn.Thread, Out: chat.Say(render.MentionMessage(m))}
		if _, err := u.cfg.Sender.Enqueue(ctx, post); err != nil {
			u.cfg.Logger.WarnContext(ctx, "live: a mention of a turn was not queued", "outbox_id", outbox.ID(post.Source, post.Role), "channel", u.turn.Channel, "err", err)
			continue
		}
		telemetry.Count(ctx, "agentkourier_mentions_total", u.turn.Binding, "made")
		if u.cfg.Audit == nil {
			continue
		}
		rec := audit.Mention{Turn: u.turn.Source, Channel: u.turn.Channel, Thread: u.turn.Thread, Name: m.Name, User: m.User}
		if err := u.cfg.Audit.Mention(ctx, audit.Actor{Binding: u.turn.Binding}, rec); err != nil {
			u.cfg.Logger.WarnContext(ctx, "live: a mention was not audited", "channel", u.turn.Channel, "err", err)
		}
	}
}

// mentioned is the declared users the turn has a row for: pending, delivered or abandoned, each of which has had its one try.
func (u *Run) mentioned(ctx context.Context) ([]string, error) {
	var users []string
	for _, m := range u.turn.Mentions {
		if slices.Contains(users, m.User) {
			continue
		}
		_, err := u.cfg.Store.GetOutbox(ctx, outbox.ID(outbox.MentionSource(u.turn.Source, m.User), store.OutboxNotice))
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
