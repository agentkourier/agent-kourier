package faketeams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Control is a client of the fake's control API, which a test drives the fake with.
type Control struct {
	base   string
	client *http.Client
}

// NewControl returns the client of the control API of the fake at base, such as http://faketeams.e2e.svc.cluster.local:8080.
func NewControl(base string) *Control {
	return &Control{base: base, client: &http.Client{Timeout: deliveryTimeout + 10*time.Second}}
}

// Healthy says whether the fake answers.
func (c *Control) Healthy(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_control/healthz", nil, nil)
}

// Install adds the bot of in.App to the team, and in.Channel to the team's channels, which Teams tells the bot with a conversationUpdate
// on the team's conversation that names no channel: the bot then knows the team's record (its General channel's), and learns a
// channel's with the first activity in it, such as Mention.
func (c *Control) Install(ctx context.Context, in Install) (Delivered, error) {
	var out Delivered
	return out, c.do(ctx, http.MethodPost, "/_control/install", in, &out)
}

// Mention posts a person's message, which mentions the bot unless in.Plain, and sends it to the bot: the answer is the message's ID
// and the bot's answer.
func (c *Control) Mention(ctx context.Context, in Mention) (Delivered, error) {
	var out Delivered
	return out, c.do(ctx, http.MethodPost, "/_control/mention", in, &out)
}

// Press presses an action of a card of a bot's message, and returns the bot's answer to the invoke.
func (c *Control) Press(ctx context.Context, in Press) (Delivered, error) {
	var out Delivered
	return out, c.do(ctx, http.MethodPost, "/_control/press", in, &out)
}

// Workflows posts another app's card into a channel, as Workflows posts an alert's, and returns its ID. No bot is sent it.
func (c *Control) Workflows(ctx context.Context, in Workflows) (string, error) {
	var out Delivered
	err := c.do(ctx, http.MethodPost, "/_control/workflows", in, &out)
	return out.ID, err
}

// Messages returns the top-level posts of channel, or the thread of root (the root first), oldest first.
func (c *Control) Messages(ctx context.Context, channel, root string) ([]MessageView, error) {
	var out []MessageView
	q := url.Values{"channel": {channel}}
	if root != "" {
		q.Set("root", root)
	}
	return out, c.do(ctx, http.MethodGet, "/_control/messages?"+q.Encode(), nil, &out)
}

// Subscriptions returns the live subscriptions.
func (c *Control) Subscriptions(ctx context.Context) ([]SubscriptionView, error) {
	var out []SubscriptionView
	return out, c.do(ctx, http.MethodGet, "/_control/subscriptions", nil, &out)
}

// Stats returns what the fake counted.
func (c *Control) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	return out, c.do(ctx, http.MethodGet, "/_control/stats", nil, &out)
}

// do makes one control call, with in as its JSON body if set, and decodes the answer into out if set. A status other than 200 is an
// error that quotes the fake's message.
func (c *Control) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("faketeams %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*maxBody))
	if err != nil {
		return fmt.Errorf("faketeams %s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("faketeams %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("faketeams %s %s: %w", method, path, err)
	}
	return nil
}
