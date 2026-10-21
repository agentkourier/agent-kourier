package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	slackgo "github.com/slack-go/slack"
)

// ErrChannelNotFound is what ChannelID returns when no channel the bot can see has the name.
var ErrChannelNotFound = errors.New("no channel with that name")

// maxChannelPages bounds ChannelID's walk: at the page size of 200, a workspace of ten thousand channels.
const maxChannelPages = 50

// ChannelID returns the ID of the public or private channel called name, which may start with # and is compared
// without regard to case; Slack's names are lower case. It reads conversations.list a page at a time, archived
// channels left out, and returns ErrChannelNotFound after the last page. A private channel is listed only if the bot
// is a member of it. It needs the scopes channels:read and groups:read. It changes nothing.
func (c *Client) ChannelID(ctx context.Context, name string) (string, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "#")
	if name == "" {
		return "", errors.New("slack channel lookup: no name")
	}
	seen := map[string]bool{}
	cursor := ""
	for range maxChannelPages {
		channels, next, err := c.api.GetConversationsContext(ctx, &slackgo.GetConversationsParameters{
			Cursor: cursor, ExcludeArchived: true, Limit: c.pageSize, Types: []string{"public_channel", "private_channel"}})
		if err != nil {
			return "", c.scrub("conversations.list", err)
		}
		for _, ch := range channels {
			if strings.EqualFold(ch.Name, name) {
				return ch.ID, nil
			}
		}
		if next == "" {
			return "", ErrChannelNotFound
		}
		if seen[next] {
			return "", fmt.Errorf("slack channel lookup: cursor %q repeats", next)
		}
		seen[next] = true
		cursor = next
	}
	return "", fmt.Errorf("slack channel lookup: the name is in none of the first %d pages; give the channel's ID", maxChannelPages)
}
