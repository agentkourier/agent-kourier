package slack

import (
	"context"
	"errors"
	"fmt"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// busyStatus is the words of the assistant status of a thread that Agent Kourier is working in. Slack puts the app's name
// before them. Agent Kourier's own, never the agent's.
const busyStatus = "is working…"

// SetThreadStatus sets the assistant status of the thread threadTS in channel to status (assistant.threads.setStatus), the
// row with a spinner at the bottom of a thread that shows who is working in it, and an empty status takes it off. Slack takes it in a
// thread that the app did not open, with no stream, and clears it itself when the app posts in the thread. It reaches only
// the clients that are open when it is set. An empty channel or threadTS is store.ErrInvalid, and no call is made. The error
// holds no token and keeps the Slack code for errors.As.
//
// UNCONFIRMED(WP-04): which scope Slack requires. slack-go's documentation of the method says it accepts chat:write or
// assistant:write, and that assistant:write is being deprecated in favor of chat:write; an app with only chat:write was
// answered ok in one probe (2026-10-18), and whether it showed the status there is not known. Also the codes of refusesStatus.
func (c *Client) SetThreadStatus(ctx context.Context, channel, threadTS, status string) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.SetThreadStatus")
	defer func() { finishChat(ctx, op, "SetThreadStatus", telemetryErr) }()
	if channel == "" || threadTS == "" {
		return fmt.Errorf("slack set thread status: the channel and the thread are required: %w", store.ErrInvalid)
	}
	err := c.api.SetAssistantThreadsStatusContext(ctx, slackgo.AssistantThreadsSetStatusParameters{ChannelID: channel, ThreadTS: threadTS, Status: status})
	if err != nil {
		return c.scrub("assistant.threads.setStatus", err)
	}
	return nil
}

// threadStatuser is what MarkBusy asks of the API: the *Client has it, and a double of it may not.
type threadStatuser interface {
	SetThreadStatus(ctx context.Context, channel, threadTS, status string) error
}

// MarkBusy implements chat.BusyMarker as the assistant status of the thread: "is working…" to mark it, and an empty status to
// take the mark off. It needs no stream and no message of the thread's. A refusal that says the app or workspace cannot show
// it (refusesStatus) turns the status off for this connection for the life of the process: it is logged once, with the way to
// enable it, and this and every later call says chat.ErrNoBusy without a call to Slack. Any other error is the call's own and
// turns nothing off. The status is a courtesy, and the caller never fails a turn for it.
func (p *Platform) MarkBusy(ctx context.Context, t chat.Target, busy bool) error {
	if t.Thread == "" {
		return fmt.Errorf("slack: %w: a status is shown in a thread", chat.ErrInvalid)
	}
	s, ok := p.c.(threadStatuser)
	if !ok || p.statusOff.Load() {
		return chat.ErrNoBusy
	}
	status := ""
	if busy {
		status = busyStatus
	}
	err := s.SetThreadStatus(ctx, t.Channel, t.Thread, status)
	if err == nil {
		return nil
	}
	var resp slackgo.SlackErrorResponse
	if errors.As(err, &resp) && refusesStatus(resp.Err) {
		p.statusOff.Store(true)
		p.statusWarned.Do(func() {
			p.log.InfoContext(ctx, "slack: Slack refused assistant.threads.setStatus, so threads are not shown as busy on this connection until restart",
				"conn", p.conn, "code", resp.Err)
		})
		return fmt.Errorf("%w: %w", chat.ErrNoBusy, err)
	}
	return err
}

// refusesStatus says whether code is Slack saying that this app, token or workspace cannot set the assistant status, a
// refusal that no retry mends: the scope is missing, the token's type or the app's feature is not allowed, or the method does not
// exist. The code is read from Slack's answer, never matched in text.
// UNCONFIRMED(WP-04): the codes, which are those Slack's documentation gives for the scope and the feature in general
// (missing_scope, not_allowed_token_type, access_denied, feature_not_enabled, no_permission) and unknown_method, which a
// Slack that lacks the method answers (the fake of the end-to-end tier does); no real refusal has been recorded.
func refusesStatus(code string) bool {
	switch code {
	case "missing_scope", "not_allowed_token_type", "not_allowed", "access_denied", "feature_not_enabled", "no_permission", "unknown_method":
		return true
	}
	return false
}
