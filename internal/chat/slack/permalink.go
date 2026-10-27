package slack

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// WorkspaceURL returns the workspace's address, such as "https://example.slack.com", which Run learned from auth.test when
// it connected and keeps, or "" before that, and when Slack's answer was no https address with a host and nothing
// more. It makes no call.
func (c *Client) WorkspaceURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

// workspaceOrigin returns the scheme and host of raw if it is an https URL with a host and no credentials, and "" if
// it is anything else. The path, query and fragment of auth.test's answer are dropped: a permalink adds its own.
func workspaceOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return "https://" + u.Host
}

// workspaceURLer is what Permalink asks of the API: the *Client has it, and a double of it may not.
type workspaceURLer interface{ WorkspaceURL() string }

var (
	channelID = regexp.MustCompile(`^[A-Z0-9]{1,32}$`)
	messageTS = regexp.MustCompile(`^[0-9]{1,16}\.[0-9]{1,16}$`)
)

// Permalink implements chat.Permalinker: "<workspace>/archives/<channel>/p<ts without its dot>", the form Slack's own
// permalinks of a message have. It is built from the workspace address the client keeps (WorkspaceURL) and makes no
// call, so it can be used where a call cannot. It is false before Run has connected, for an API that keeps no
// address, and for a Ref whose channel or ts is not the shape Slack's are.
func (p *Platform) Permalink(r chat.Ref) (string, bool) {
	w, ok := p.c.(workspaceURLer)
	if !ok || !channelID.MatchString(r.Channel) || !messageTS.MatchString(r.ID) {
		return "", false
	}
	base := w.WorkspaceURL()
	if base == "" {
		return "", false
	}
	return base + "/archives/" + r.Channel + "/p" + strings.Replace(r.ID, ".", "", 1), true
}
