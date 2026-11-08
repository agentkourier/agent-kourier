package fakeslack

import (
	"encoding/json"
	"slices"
	"strings"
)

// WithoutOwnPostEvents stops the fake sending Socket Mode events for the bot's
// own posts and updates. By default every bot post is delivered as a message
// event, and every update as a message_changed event, to a connected client, as
// Slack does; Agent Kourier has to recognise and ignore them. With several apps
// (WithApp) it also stops the events other apps in the channel get for the post.
// Set at construction, read-only after.
func WithoutOwnPostEvents() Option { return func(s *Server) { s.noOwnEvents = true } }

// ownEventMethods are the Web API methods that queue events for the bot's own
// posts. Each holds ownMu from dispatch until its events are sent.
var ownEventMethods = map[string]bool{
	"chat.postMessage": true, "chat.update": true, "chat.startStream": true, "chat.stopStream": true,
}

// ownKey names a bot message, for the envelopes of its events.
type ownKey struct{ channel, ts string }

// ownRecord is the envelopes sent for one bot message: the AppID that posted it, and the envelope_id of each event
// by the AppID of the app it went to.
type ownRecord struct {
	poster string
	sent   map[string][]string
}

// OwnEventEnvelopes returns the envelope_id of each event the fake sent to the app that posted the bot message
// ts in channel, in order: its message event, then one message_changed for each update (or the stop of a
// stream). A test that wants to know Agent Kourier has seen its own post's event passes them to WaitAck. They
// are recorded before the call that caused them returns. The list is empty when events are off or no client was
// connected. EventEnvelopes names the app.
func (s *Server) OwnEventEnvelopes(channel, ts string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ownSent[ownKey{channel, ts}]
	if rec == nil {
		return nil
	}
	return slices.Clone(rec.sent[rec.poster])
}

// EventEnvelopes is OwnEventEnvelopes for the events that went to the app appID, which may be another app than
// the one that posted the message: the other apps whose bots are in the channel get the same events.
func (s *Server) EventEnvelopes(appID, channel, ts string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ownSent[ownKey{channel, ts}]
	if rec == nil {
		return nil
	}
	return slices.Clone(rec.sent[appID])
}

// ownEvent is a bot post (prev nil) or change to one, waiting to be sent.
type ownEvent struct {
	channel   string
	prev      *Message // the message before a change, without metadata
	cur       Message  // the message now, without metadata
	changedTS string   // the ts of a message_changed event
}

// queueOwnLocked records that the bot posted cur in channel, or, if prev is not
// nil, changed it from prev. The events go out from flushOwn, once the request
// has released s.mu; the request holds ownMu throughout. Callers hold s.mu.
func (s *Server) queueOwnLocked(channel string, prev, cur *Message) {
	if s.noOwnEvents {
		return
	}
	e := ownEvent{channel: channel, cur: cur.clone()}
	e.cur.Metadata = nil
	if prev != nil {
		p := prev.clone()
		p.Metadata = nil
		e.prev, e.changedTS = &p, s.nextTSLocked()
	}
	s.ownPending = append(s.ownPending, e)
}

// flushOwn sends the events the request that has just been handled queued, in
// order, and records their envelope IDs. The request holds ownMu, so the queue
// holds only its events. With no client connected they are dropped, as Slack
// drops events for an app that is not listening.
//
// Each event goes to the app that posted, first, and then to every other app
// that is in the channel (recipients), each over its own connection and in an
// envelope made for it. The others see the post as a bot message: a message
// event with the poster's user, bot_id and app_id, and one whose bot the post
// mentions is sent an app_mention with the same fields before it. That order is the fake's choice: both events carry the same ts, and the probe found no fixed
// order between them, so no client may rely on one.
//
// UNCONFIRMED(WP-04): a message event for each bot post with no subtype, and
// with bot_id and app_id; that blocks are included and metadata is not; a
// message_changed event, hidden, with its own ts, for each chat.update; for
// streaming, a message event at chat.startStream, no event at chat.appendStream
// and a message_changed at chat.stopStream; and that a reply gives no
// message_changed for its thread parent, where Slack sends one, since the
// parent's reply_count changes. UNCONFIRMED(MC-S): that an app in the channel gets
// another app's posts, and their updates, as events of this shape (a message with
// the poster's user, bot_id and app_id, and a message_changed), where Slack may
// leave out the updates or send a bot_message subtype; a live probe settles it.
func (s *Server) flushOwn() {
	s.mu.Lock()
	events := s.ownPending
	s.ownPending = nil
	s.mu.Unlock()
	for _, e := range events {
		poster := s.appOrDefault(e.cur.AppID)
		targets := []App{poster}
		for _, a := range s.recipients(e.channel) {
			if a.AppID != poster.AppID {
				targets = append(targets, a)
			}
		}
		body := s.ownEventBody(e)
		for _, a := range targets {
			// An app whose bot the post mentions is sent an app_mention first, as a person's post is (userMessage), with the poster's bot_id.
			bodies := []map[string]any{body}
			if a.AppID != poster.AppID && mentionsBot(e, a) {
				bodies = []map[string]any{s.appMentionBody(e), body}
			}
			for _, b := range bodies {
				id, err := s.eventCallbackTo(a, b)
				if err != nil {
					continue // ErrNoConnection is the dropped case
				}
				s.mu.Lock()
				k := ownKey{e.channel, e.cur.TS}
				if s.ownSent[k] == nil {
					s.ownSent[k] = &ownRecord{poster: poster.AppID, sent: map[string][]string{}}
				}
				s.ownSent[k].sent[a.AppID] = append(s.ownSent[k].sent[a.AppID], id)
				s.mu.Unlock()
			}
		}
	}
}

// blockOf is the part of a Block Kit block that a mention can be in: the mrkdwn text of a section or the mrkdwn elements of a context block, which is how
// render.MentionMessage mentions a bot.
type blockOf struct {
	Type     string            `json:"type"`
	Text     *blockText        `json:"text"`
	Fields   []blockText       `json:"fields"`
	Elements []json.RawMessage `json:"elements"`
}

type blockText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// tokenFor says whether text holds the user token of bot, <@U> or <@U|label>.
func tokenFor(text, bot string) bool {
	return strings.Contains(text, "<@"+bot+">") || strings.Contains(text, "<@"+bot+"|")
}

// mentionsBot says whether Slack sends the app an app_mention for the bot post e, by the rule that docs/spikes/slack-mention-probe.md ("What a second app
// receives", 2026-11-03) measured:
//   - a token in the top-level text gives one (rows B, C, D), whatever the blocks;
//   - a token in the mrkdwn text of a section or context block gives one (rows A, F), but only when the post has no rich_text and no markdown block: with one
//     the token in a block gives none (row E, a markdown block);
//   - a rich_text user element gives none (row G), and a token of another user none (row H).
//
// A direct message gets no app_mention, as a person's does not (Mention). An edit gets none here: Slack sends an app_mention with edited set for a bot's edit
// that adds a mention (row I), and the fake does not, a known departure; the adapter drops an edited mention.
//
// UNCONFIRMED(MC-S): the cases the probe did not cover: a Slack Connect channel, Enterprise Grid, an app that mentions itself, a token in the fields of a
// section block (read like its text), and any block type but section, context, rich_text and markdown.
func mentionsBot(e ownEvent, a App) bool {
	if e.prev != nil || strings.HasPrefix(e.channel, "D") {
		return false
	}
	if tokenFor(e.cur.Text, a.BotUser) {
		return true
	}
	var blocks []blockOf
	if len(e.cur.Blocks) == 0 || json.Unmarshal(e.cur.Blocks, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "rich_text" || b.Type == "markdown" {
			return false
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "section":
			if b.Text != nil && b.Text.Type == "mrkdwn" && tokenFor(b.Text.Text, a.BotUser) {
				return true
			}
			for _, f := range b.Fields {
				if f.Type == "mrkdwn" && tokenFor(f.Text, a.BotUser) {
					return true
				}
			}
		case "context":
			for _, raw := range b.Elements {
				var el blockText
				if json.Unmarshal(raw, &el) == nil && el.Type == "mrkdwn" && tokenFor(el.Text, a.BotUser) {
					return true
				}
			}
		}
	}
	return false
}

// appMentionBody builds the app_mention event for the bot post e: the message's own fields, with the poster's user, bot_id and app_id.
//
// UNCONFIRMED(MC-S): the fields of an app_mention for a bot's post beyond the message's own (user, bot_id, app_id, text, ts, channel); the probe lists bot_profile too.
func (s *Server) appMentionBody(e ownEvent) map[string]any {
	ev := map[string]any{
		"type": "app_mention", "user": e.cur.User, "bot_id": e.cur.BotID, "app_id": e.cur.AppID, "text": e.cur.Text,
		"ts": e.cur.TS, "event_ts": e.cur.TS, "channel": e.channel,
	}
	if e.cur.ThreadTS != "" {
		ev["thread_ts"] = e.cur.ThreadTS
	}
	if e.cur.Blocks != nil {
		ev["blocks"] = e.cur.Blocks
	}
	return ev
}

// ownEventBody builds the Events API event for e.
func (s *Server) ownEventBody(e ownEvent) map[string]any {
	chType := s.channelType(e.channel)
	if e.prev != nil {
		return map[string]any{
			"type": "message", "subtype": "message_changed", "hidden": true, "channel": e.channel, "channel_type": chType,
			"ts": e.changedTS, "event_ts": e.changedTS, "message": e.cur, "previous_message": *e.prev,
		}
	}
	ev := map[string]any{
		"type": "message", "user": e.cur.User, "bot_id": e.cur.BotID, "app_id": e.cur.AppID, "text": e.cur.Text,
		"ts": e.cur.TS, "event_ts": e.cur.TS, "channel": e.channel, "channel_type": chType,
	}
	if e.cur.ThreadTS != "" {
		ev["thread_ts"] = e.cur.ThreadTS
	}
	if e.cur.Blocks != nil {
		ev["blocks"] = e.cur.Blocks
	}
	return ev
}
