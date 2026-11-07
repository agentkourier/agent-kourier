package slack_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

func TestGroupsReadable(t *testing.T) {
	_, c, calls := groupsClient(t, nil)
	for _, tc := range []struct {
		name   string
		groups []string
		reads  int    // usergroups.users.list calls
		failed string // the group the error names, if there is an error
	}{
		{"no groups makes no call", nil, 0, ""},
		{"one group", []string{"S1"}, 1, ""},
		{"a group with no members is readable", []string{"S3"}, 1, ""},
		{"every group is read, in order", []string{"S1", "S2", "S3"}, 3, ""},
		{"the first group that is not there ends the call", []string{"S1", "SNONE", "S2"}, 2, "SNONE"},
		{"a group before it is read, and one after is not", []string{"SNONE", "S1"}, 1, "SNONE"},
	} {
		before := calls.count("/usergroups.users.list")
		err := c.GroupsReadable(t.Context(), tc.groups)
		if used := calls.count("/usergroups.users.list") - before; used != tc.reads {
			t.Errorf("%s: %d reads, want %d", tc.name, used, tc.reads)
		}
		var ge *chat.GroupError
		switch {
		case tc.failed == "" && err != nil:
			t.Errorf("%s: GroupsReadable = %v, want nil", tc.name, err)
		case tc.failed != "" && (!errors.As(err, &ge) || ge.Group != tc.failed):
			t.Errorf("%s: GroupsReadable = %v, want a *chat.GroupError that names %s", tc.name, err, tc.failed)
		}
	}
}

// The members are not asked about: the call needs every group to answer and nothing of who is in it, and it takes no one's word
// that the group is current. Disabled groups are not asked for, as IsInAnyUserGroup does not.
func TestGroupsReadableAsksSlackForEachGroupAndDoesNotCache(t *testing.T) {
	fake, c, calls := groupsClient(t, nil)
	if err := c.GroupsReadable(t.Context(), []string{"S1", "S2"}); err != nil {
		t.Fatal(err)
	}
	f := calls.sent("/usergroups.users.list")
	if len(f) != 2 || f[0].Get("usergroup") != "S1" || f[1].Get("usergroup") != "S2" || f[0].Has("include_disabled") || f[1].Has("include_disabled") {
		t.Errorf("requests %v; want one for S1 and one for S2, with no include_disabled", f)
	}
	if n := len(calls.n); n != 1 {
		t.Errorf("the client called %d methods, %v; want usergroups.users.list alone", n, calls.n)
	}
	// A group that is gone shows at the next call: nothing is kept between calls.
	fake.SetUserGroup("S1", "U1")
	if err := c.GroupsReadable(t.Context(), []string{"S1"}); err != nil {
		t.Fatal(err)
	}
	if n := calls.count("/usergroups.users.list"); n != 3 {
		t.Errorf("%d calls for three reads; want 3, none served from a cache", n)
	}
}

// TestGroupsReadableSaysWhichFailuresCanNeverSucceed: the codes of usergroups.users.list that only an operator can mend wrap
// chat.ErrGroupsMisconfigured, as IsInAnyGroup's do; a group that is not there also wraps chat.ErrGroupNotFound and an app
// without the scope chat.ErrMissingScope, and no other code wraps those. Everything that may pass on a retry wraps none of the three.
func TestGroupsReadableSaysWhichFailuresCanNeverSucceed(t *testing.T) {
	slackError := func(code string) string { return `{"ok":false,"error":"` + code + `"}` }
	type kind struct{ misconfigured, notFound, scope bool }
	for name, tc := range map[string]struct {
		status int
		body   string
		err    error
		hang   bool
		want   kind
		fatal  bool
	}{
		"a group that does not exist":    {status: http.StatusOK, body: slackError("no_such_subteam"), want: kind{true, true, false}},
		"an app without the scope":       {status: http.StatusOK, body: slackError("missing_scope"), want: kind{true, false, true}},
		"a plan without user groups":     {status: http.StatusOK, body: slackError("plan_upgrade_required"), want: kind{misconfigured: true}},
		"a token type the method denies": {status: http.StatusOK, body: slackError("not_allowed_token_type"), want: kind{misconfigured: true}},
		"a token without permission":     {status: http.StatusOK, body: slackError("no_permission"), want: kind{misconfigured: true}},
		"an enterprise grid":             {status: http.StatusOK, body: slackError("enterprise_is_restricted"), want: kind{misconfigured: true}},
		"a rate limit as a 429":          {status: http.StatusTooManyRequests},
		"a rate limit as a code":         {status: http.StatusOK, body: slackError("ratelimited")},
		"a 500":                          {status: http.StatusInternalServerError, body: "oops"},
		"an internal error":              {status: http.StatusOK, body: slackError("internal_error")},
		"a rejected token":               {status: http.StatusOK, body: slackError("invalid_auth"), fatal: true},
		"a network error":                {err: errors.New("connection reset")},
		"a timeout":                      {hang: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, c, calls := groupsClient(t, func(cfg *slack.Config) { cfg.AuthzTimeout = 200 * time.Millisecond })
			calls.status, calls.body, calls.err, calls.hang = tc.status, tc.body, tc.err, tc.hang
			err := c.GroupsReadable(t.Context(), []string{"S1"})
			var ge *chat.GroupError
			if err == nil || !errors.As(err, &ge) || ge.Group != "S1" {
				t.Fatalf("GroupsReadable = %v, want a *chat.GroupError for S1", err)
			}
			got := kind{errors.Is(err, chat.ErrGroupsMisconfigured), errors.Is(err, chat.ErrGroupNotFound), errors.Is(err, chat.ErrMissingScope)}
			if got != tc.want {
				t.Errorf("error %v wraps (misconfigured, not found, scope) = %v; want %v", err, got, tc.want)
			}
			if fatal := slack.NewPlatform(c, "ns/conn").Classify(chat.OpOther, err).Class == chat.FatalAuth; fatal != tc.fatal {
				t.Errorf("Classify says FatalAuth = %t; want %t", fatal, tc.fatal)
			}
		})
	}
}

func TestAGroupsReadableErrorKeepsTheSlackCodeAndNoToken(t *testing.T) {
	const bot = "canary-bot-token"
	_, c, calls := groupsClient(t, func(cfg *slack.Config) { cfg.BotToken = secret.New(bot) })
	calls.status, calls.body = http.StatusOK, `{"ok":false,"error":"missing_scope"}`
	err := c.GroupsReadable(t.Context(), []string{"S1"})
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) || resp.Err != "missing_scope" || strings.Contains(err.Error(), bot) || !strings.Contains(err.Error(), "S1") {
		t.Errorf("error %v; want Slack's code missing_scope, the group and no token", err)
	}
}

func TestACancelledGroupsReadIsNotPermanent(t *testing.T) {
	_, c, _ := groupsClient(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := c.GroupsReadable(ctx, []string{"S1"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, chat.ErrGroupsMisconfigured) || errors.Is(err, chat.ErrGroupNotFound) || errors.Is(err, chat.ErrMissingScope) {
		t.Errorf("GroupsReadable on a cancelled context = %v; want a cancellation that is not the permanent kind", err)
	}
}

// TestGroupsReadableIsBoundedAsAWhole: a Slack that never answers cannot hold a probe, and the bound is for all the groups
// together, as IsInAnyUserGroup's is.
func TestGroupsReadableIsBoundedAsAWhole(t *testing.T) {
	_, c, calls := groupsClient(t, func(cfg *slack.Config) { cfg.AuthzTimeout = 250 * time.Millisecond })
	calls.delay = 150 * time.Millisecond
	err := c.GroupsReadable(t.Context(), []string{"S1", "S2", "S3"})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, chat.ErrGroupsMisconfigured) {
		t.Errorf("GroupsReadable across three 150 ms reads = %v; want a deadline error that is not the permanent kind", err)
	}
	if n := calls.count("/usergroups.users.list"); n > 2 {
		t.Errorf("%d reads; want the bound to end the call before a third", n)
	}
}

// The adapter's Platform answers the same: it is the chat.Platform method.
func TestPlatformGroupsReadableIsTheClientsRead(t *testing.T) {
	_, c, _ := groupsClient(t, nil)
	var p chat.Platform = slack.NewPlatform(c, "ns/conn")
	if err := p.GroupsReadable(t.Context(), []string{"S1"}); err != nil {
		t.Errorf("GroupsReadable of a group that is there = %v", err)
	}
	if err := p.GroupsReadable(t.Context(), []string{"SNONE"}); !errors.Is(err, chat.ErrGroupNotFound) {
		t.Errorf("GroupsReadable of a group that is not there = %v, want chat.ErrGroupNotFound", err)
	}
}
