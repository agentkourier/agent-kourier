package fakeslack

import (
	"slices"
)

// Attachment is a legacy Slack attachment, the one an incoming webhook posts its content in, with the
// fields Agent Kourier reads. The JSON names are Slack's, so a control request and the message the fake stores
// and delivers have the same shape.
type Attachment struct {
	// ID is the attachment's position in its message, from 1. Slack assigns it, and so does the fake:
	// whatever a caller sets is replaced.
	ID        int    `json:"id,omitempty"`
	Color     string `json:"color,omitempty"`
	Fallback  string `json:"fallback,omitempty"`
	Title     string `json:"title,omitempty"`
	TitleLink string `json:"title_link,omitempty"`
	Text      string `json:"text,omitempty"`
	Footer    string `json:"footer,omitempty"`
	// MarkdownIn names the fields Slack renders as mrkdwn ("text", "pretext", "fields"). The text itself
	// is the same raw string either way.
	MarkdownIn []string `json:"mrkdwn_in,omitempty"`
}

// WebhookPost is a post through an incoming webhook: what Alertmanager's Slack receiver does, and so what
// the alert channel holds. It has a bot ID, no user and no app_id, and its content is in legacy
// attachments, with the message's own text usually empty.
type WebhookPost struct {
	Channel string `json:"channel"`
	// BotID is the bot the webhook posts as, such as B0123456789. It may be the fake's own bot, which is how
	// a test shows Agent Kourier ignoring itself.
	BotID string `json:"bot_id"`
	// Username is the name the post shows; "incoming-webhook" if empty.
	Username    string       `json:"username,omitempty"`
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// defaultWebhookName is the username of a webhook post that names none.
const defaultWebhookName = "incoming-webhook"

// Webhook stores a post through an incoming webhook in its channel and delivers the message event for it,
// as Slack does for an app subscribed to message.channels that is in the channel. Like a user's message it
// needs a client connected to receive the event: with none, nothing is stored and the error is
// ErrNoConnection. The post must have a channel and a bot ID, and some content: text, or an attachment with
// a field set.
//
// UNCONFIRMED(WP-04): the event's fields: type message, subtype bot_message, bot_id, username, the text
// (present even when empty), the attachments with an id each, channel_type, and neither user nor app_id.
// They come from the history of the alert channel (conversations.history), not from a captured event.
// The post is always top-level, though a real webhook can post into a thread.
func (s *Server) Webhook(p WebhookPost) (UserMessage, error) {
	if p.Channel == "" || p.BotID == "" {
		return UserMessage{}, invalid("fakeslack: a webhook post needs a channel and a bot ID")
	}
	if p.Text == "" && !slices.ContainsFunc(p.Attachments, hasContent) {
		return UserMessage{}, invalid("fakeslack: a webhook post needs text, or an attachment with content")
	}
	if slices.ContainsFunc(p.Attachments, func(a Attachment) bool { return !hasContent(a) }) {
		return UserMessage{}, invalid("fakeslack: a webhook post's attachment has no content")
	}
	if s.Connections() == 0 {
		return UserMessage{}, ErrNoConnection
	}
	attachments := cloneAttachments(p.Attachments)
	for i := range attachments {
		attachments[i].ID = i + 1
	}
	m := &Message{Type: "message", Subtype: "bot_message", BotID: p.BotID, Username: p.Username, Text: p.Text, Attachments: attachments}
	if m.Username == "" {
		m.Username = defaultWebhookName
	}
	s.mu.Lock()
	_ = s.addLocked(p.Channel, m) // only a reply can fail, and a webhook post is top-level
	s.mu.Unlock()
	out := UserMessage{Channel: p.Channel, TS: m.TS}
	ev := map[string]any{
		"type": "message", "subtype": "bot_message", "bot_id": m.BotID, "username": m.Username, "text": m.Text,
		"ts": m.TS, "event_ts": m.TS, "channel": p.Channel, "channel_type": s.channelType(p.Channel),
	}
	if len(attachments) > 0 {
		ev["attachments"] = attachments
	}
	id, err := s.eventCallback(ev)
	if err != nil {
		return out, err
	}
	out.Envelopes = append(out.Envelopes, id)
	return out, nil
}

// hasContent says whether an attachment carries anything a reader would see.
func hasContent(a Attachment) bool {
	return a.Fallback != "" || a.Title != "" || a.TitleLink != "" || a.Text != "" || a.Footer != ""
}

// cloneAttachments returns a deep copy of as, nil for none.
func cloneAttachments(as []Attachment) []Attachment {
	if as == nil {
		return nil
	}
	out := slices.Clone(as)
	for i := range out {
		out[i].MarkdownIn = slices.Clone(out[i].MarkdownIn)
	}
	return out
}
