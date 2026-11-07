package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// groupsFake is a groupReader with an answer, a classification and a record of its calls.
type groupsFake struct {
	mu      sync.Mutex
	err     error
	verdict chat.Verdict
	wait    bool // block until the context ends
	calls   [][]string
}

func (g *groupsFake) GroupsReadable(ctx context.Context, groups []string) error {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string(nil), groups...))
	err, wait := g.err, g.wait
	g.mu.Unlock()
	if wait {
		<-ctx.Done()
		return &chat.GroupError{Group: groups[0], Err: fmt.Errorf("usergroups.users.list: %w", ctx.Err())}
	}
	return err
}

func (g *groupsFake) Classify(chat.Op, error) chat.Verdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.verdict
}

func (g *groupsFake) count() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.calls) }

// groupErr is the error of a GroupsReadable that failed at group, wrapping what the platform wrapped.
func groupErr(group string, wraps ...error) error {
	err := errors.New("usergroups.users.list: some_code")
	for _, w := range wraps {
		err = fmt.Errorf("%w: %w", w, err)
	}
	return &chat.GroupError{Group: group, Err: err}
}

var approverGroups = []string{"S0AAAAAAAAA", "S0BBBBBBBBB", "S0CCCCCCCCC"}

// What an approver group that the bot cannot read means for ApproversReadable, every row of the contract: the two sentinels are False,
// any other permanent refusal is False/GroupsUnreadable, a credential the platform rejects is False/ChatCredentialsRejected, and every
// other failure is Unknown/ChatUnavailable. The message names the group that failed by its place in the Binding's own list and its ID, and
// nothing else.
func TestGroupCheckerSaysWhatTheErrorMeans(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		verdict chat.Verdict
		want    want
		message string // a substring of the Message
	}{
		{"every group is readable", nil, chat.Verdict{}, want{bindingcheck.True, bindingcheck.ReasonOK}, ""},
		{"a group that is not there", groupErr(approverGroups[1], chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured), chat.Verdict{}, want{bindingcheck.False, bindingcheck.ReasonGroupNotFound},
			"approverGroups[1] (S0BBBBBBBBB) does not exist"},
		{"the scope is missing", groupErr(approverGroups[0], chat.ErrMissingScope, chat.ErrGroupsMisconfigured), chat.Verdict{Class: chat.Permanent}, want{bindingcheck.False, bindingcheck.ReasonMissingScope},
			"lacks the permission to read user groups"},
		{"the workspace, plan or token refuses", groupErr(approverGroups[2], chat.ErrGroupsMisconfigured), chat.Verdict{}, want{bindingcheck.False, bindingcheck.ReasonGroupsUnreadable},
			"approverGroups[2] (S0CCCCCCCCC)"},
		{"the token is rejected", groupErr(approverGroups[0], errors.New("slack: invalid_auth")), chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}, want{bindingcheck.False, bindingcheck.ReasonChatCredentialsRejected},
			"rejected the bot's token"},
		{"rate limited", groupErr(approverGroups[0], errors.New("slack: ratelimited")), chat.Verdict{Class: chat.Transient, RetryAfter: 30 * time.Second}, want{bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable},
			"rate-limited"},
		{"a transient failure", groupErr(approverGroups[0], errors.New("slack: 502")), chat.Verdict{Class: chat.Transient}, want{bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable},
			msgChatReadFailed},
		{"a failure that is not the group's", errors.New("slack: surprise"), chat.Verdict{Class: chat.Transient}, want{bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable},
			msgChatReadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &groupsFake{err: tc.err, verdict: tc.verdict}
			out, err := groupChecker{platform: f, timeout: time.Second}.CheckGroups(t.Context(), approverGroups)
			if err != nil || out.Status != tc.want.status || out.Reason != tc.want.reason {
				t.Fatalf("CheckGroups = %+v, %v; want %s/%s", out, err, tc.want.status, tc.want.reason)
			}
			if !strings.Contains(out.Message, tc.message) {
				t.Errorf("message %q; want %q in it", out.Message, tc.message)
			}
			if out.Message == "" && out.Status != bindingcheck.True {
				t.Error("the outcome has no message")
			}
			if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], approverGroups) {
				t.Errorf("platform calls = %v; want one, over the Binding's groups in order", f.calls)
			}
		})
	}
}

// The Message reaches the status of the Binding, which its owner reads, so it holds only the Binding's own words: the group it names is
// one of the groups it was given, and nothing the platform said or the error held (a host, a code, a token) is in it. The cause is in the
// Cause, for the log, when the Message leaves it out.
func TestGroupCheckerMessageHoldsNoOutsideText(t *testing.T) {
	const outside = "https://internal.example.invalid/x?token=CANARY"
	for name, err := range map[string]error{
		"a group not in the Binding": &chat.GroupError{Group: "SOUTSIDER00", Err: fmt.Errorf("%s: %w: %w", outside, chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured)},
		"a refusal":                  &chat.GroupError{Group: approverGroups[0], Err: fmt.Errorf("%s: %w", outside, chat.ErrGroupsMisconfigured)},
		"a failed read":              &chat.GroupError{Group: approverGroups[0], Err: errors.New(outside)},
		"a plain error":              errors.New(outside),
	} {
		t.Run(name, func(t *testing.T) {
			out, cerr := groupChecker{platform: &groupsFake{err: err}, timeout: time.Second}.CheckGroups(t.Context(), approverGroups)
			if cerr != nil {
				t.Fatal(cerr)
			}
			if strings.Contains(out.Message, "internal.example") || strings.Contains(out.Message, "CANARY") || strings.Contains(out.Message, "SOUTSIDER00") {
				t.Errorf("message %q holds text from outside the Binding", out.Message)
			}
		})
	}
	// A group that is not one the checker was given is named by neither place nor ID.
	out, _ := groupChecker{platform: &groupsFake{err: groupErr("SOUTSIDER00", chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured)}, timeout: time.Second}.CheckGroups(t.Context(), approverGroups)
	if out.Reason != bindingcheck.ReasonGroupNotFound || !strings.Contains(out.Message, "an approver group") {
		t.Errorf("CheckGroups = %+v; want GroupNotFound that names no group", out)
	}
	// What the platform said is for the log.
	out, _ = groupChecker{platform: &groupsFake{err: groupErr(approverGroups[0], chat.ErrGroupsMisconfigured)}, timeout: time.Second}.CheckGroups(t.Context(), approverGroups)
	if !strings.Contains(out.Cause, "some_code") {
		t.Errorf("Cause %q; want the platform's code, for the log", out.Cause)
	}
}

// The checker's own bound on a chat platform that does not answer is Unknown/ChatUnavailable; the context of the Prober ending is no
// verdict at all.
func TestGroupCheckerDeadlines(t *testing.T) {
	f := &groupsFake{wait: true}
	out, err := groupChecker{platform: f, timeout: 20 * time.Millisecond}.CheckGroups(t.Context(), approverGroups)
	if err != nil || out.Status != bindingcheck.Unknown || out.Reason != bindingcheck.ReasonChatUnavailable {
		t.Errorf("CheckGroups past its own bound = %+v, %v; want Unknown/ChatUnavailable", out, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	out, err = groupChecker{platform: f, timeout: time.Minute}.CheckGroups(ctx, approverGroups)
	if err == nil || !reflect.DeepEqual(out, bindingcheck.Outcome{}) {
		t.Errorf("CheckGroups past the Prober's = %+v, %v; want an error and no outcome", out, err)
	}
}

// The chat credentials are compared as they are for the channel: a Secret that differs from the tokens in use holds the read back, and
// the platform is not asked.
func TestGroupCheckerWithholdsTheReadWhenTheChatCredentialsDiffer(t *testing.T) {
	const bot, app, canary = "xoxb-in-use", "xapp-in-use", "xoxb-NEW-CANARY"
	for _, tc := range []struct {
		name   string
		store  *secretStore
		reason bindingcheck.Reason
	}{
		{"rotated", &secretStore{vals: map[string]string{"botToken": canary, "appToken": app}}, bindingcheck.ReasonChatCredentialsChanged},
		{"gone", &secretStore{missing: true}, bindingcheck.ReasonChatCredentialsUnusable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &groupsFake{}
			c := groupChecker{platform: f, timeout: time.Second, creds: credentialsWatch{
				baseline: tokens(bot, app), secrets: tc.store,
				where: func() (string, string, bool) { return "agent-kourier-system", "slack-main", true },
			}}
			out, err := c.CheckGroups(t.Context(), approverGroups)
			if err != nil || out.Status != bindingcheck.Unknown || out.Reason != tc.reason || f.count() != 0 || strings.Contains(out.Message, canary) {
				t.Errorf("CheckGroups = %+v, %v after %d platform calls; want Unknown/%s, no token in the message, and no call", out, err, f.count(), tc.reason)
			}
		})
	}
	// The same tokens: the read goes on.
	f := &groupsFake{}
	same := credentialsWatch{baseline: tokens(bot, app), secrets: &secretStore{vals: map[string]string{"botToken": bot, "appToken": app}},
		where: func() (string, string, bool) { return "agent-kourier-system", "slack-main", true }}
	if out, err := (groupChecker{platform: f, timeout: time.Second, creds: same}).CheckGroups(t.Context(), approverGroups); err != nil || out.Status != bindingcheck.True || f.count() != 1 {
		t.Errorf("CheckGroups with the same tokens = %+v, %v after %d calls; want True and one", out, err, f.count())
	}
}

// The checker against the fake platform and its own Classify, so that the sentinels and the FatalAuth are the platform's, not ones the
// test made up.
func TestGroupCheckerAgainstTheFakePlatform(t *testing.T) {
	p := chattest.NewFake()
	p.SetGroup("S0AAAAAAAAA", "U1")
	c := groupChecker{platform: p, timeout: time.Second}
	for groups, w := range map[string]want{
		"S0AAAAAAAAA":             {bindingcheck.True, bindingcheck.ReasonOK},
		"S0AAAAAAAAA,S0NOTTHERE0": {bindingcheck.False, bindingcheck.ReasonGroupNotFound},
	} {
		if out, err := c.CheckGroups(t.Context(), strings.Split(groups, ",")); err != nil || out.Status != w.status || out.Reason != w.reason {
			t.Errorf("CheckGroups(%s) = %+v, %v; want %s/%s", groups, out, err, w.status, w.reason)
		}
	}
	p.FailNext("GroupsReadable", &chattest.FakeError{Verdict: chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}, Msg: "invalid_auth"})
	if out, err := c.CheckGroups(t.Context(), []string{"S0AAAAAAAAA"}); err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonChatCredentialsRejected {
		t.Errorf("CheckGroups with a rejected token = %+v, %v; want False/ChatCredentialsRejected", out, err)
	}
	p.FailNext("GroupsReadable", &chat.GroupError{Group: "S0AAAAAAAAA", Err: fmt.Errorf("fake: %w: %w", chat.ErrMissingScope, chat.ErrGroupsMisconfigured)})
	if out, err := c.CheckGroups(t.Context(), []string{"S0AAAAAAAAA"}); err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonMissingScope {
		t.Errorf("CheckGroups without the scope = %+v, %v; want False/MissingScope", out, err)
	}
}

// Through the real Slack client and its Classify: every code of usergroups.users.list lands on the reason the docs name, with no token
// and no code in the Message, and the code in the Cause where the Message leaves it out.
func TestGroupCheckerThroughTheRealSlackClient(t *testing.T) {
	const bot, app = "xoxb-TESTTOKEN-123456", "xapp-TESTTOKEN-654321"
	var answer atomic.Value
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch a, _ := answer.Load().(string); a {
		case "hang":
			<-release
		case "ratelimit":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case "http500":
			w.WriteHeader(http.StatusInternalServerError)
		case "ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"users":["U1"]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":false,"error":"`+a+`"}`)
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	client := slack.New(slack.Config{BotToken: secret.New(bot), AppToken: secret.New(app), APIURL: srv.URL + "/", Logger: slog.New(slog.DiscardHandler)})
	c := groupChecker{platform: slack.NewPlatform(client, "ns/conn"), timeout: 300 * time.Millisecond}

	for _, tc := range []struct {
		answer  string
		status  bindingcheck.Status
		reason  bindingcheck.Reason
		message string // a substring of the Message
		cause   string // a substring of the Cause
	}{
		{"ok", bindingcheck.True, bindingcheck.ReasonOK, "", ""},
		{"no_such_subteam", bindingcheck.False, bindingcheck.ReasonGroupNotFound, "does not exist", ""},
		{"missing_scope", bindingcheck.False, bindingcheck.ReasonMissingScope, "lacks the permission", ""},
		{"plan_upgrade_required", bindingcheck.False, bindingcheck.ReasonGroupsUnreadable, "approverGroups[0]", "plan_upgrade_required"},
		{"not_allowed_token_type", bindingcheck.False, bindingcheck.ReasonGroupsUnreadable, "approverGroups[0]", "not_allowed_token_type"},
		{"no_permission", bindingcheck.False, bindingcheck.ReasonGroupsUnreadable, "approverGroups[0]", "no_permission"},
		{"enterprise_is_restricted", bindingcheck.False, bindingcheck.ReasonGroupsUnreadable, "approverGroups[0]", "enterprise_is_restricted"},
		{"invalid_auth", bindingcheck.False, bindingcheck.ReasonChatCredentialsRejected, "rejected the bot's token", ""},
		{"ratelimit", bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable, "rate-limited", ""},
		{"http500", bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable, "read failed", ""},
		{"hang", bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable, "did not answer the read in time", ""},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			answer.Store(tc.answer)
			out, err := c.CheckGroups(t.Context(), approverGroups[:1])
			if err != nil || out.Status != tc.status || out.Reason != tc.reason || !strings.Contains(out.Message, tc.message) || !strings.Contains(out.Cause, tc.cause) {
				t.Errorf("CheckGroups = %+v, %v; want %s/%s with %q in the message and %q in the cause", out, err, tc.status, tc.reason, tc.message, tc.cause)
			}
			if tc.cause != "" && strings.Contains(out.Message, tc.cause) {
				t.Errorf("the message %q holds the code, which is for the log", out.Message)
			}
			for _, token := range []string{bot, app, "TESTTOKEN"} {
				if strings.Contains(out.Message, token) || strings.Contains(out.Cause, token) {
					t.Errorf("the message %q or the cause %q holds a token", out.Message, out.Cause)
				}
			}
		})
	}
}

// ApproversReadable is timed under its own bucket of the probe-duration histogram, whatever the probe found, and the Prober the broker
// builds probes the groups of the Bindings that name them with the served connection's bot: one call over all of a Binding's groups,
// none for a Binding with no groups.
func TestTheBrokersProberProbesApproverGroupsAndTimesThem(t *testing.T) {
	current := &snaps{}
	current.p.Store(snapshotWith(t, map[string]int64{"sre/with": 1, "sre/without": 1}, nil, map[string][]string{"sre/with": approverGroups}))
	plat := &accessFake{access: chat.Access{Member: true}, groupsErr: groupErr(approverGroups[1], chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured)}
	m := mustMetrics(t, nil)
	p := verification{
		Bindings: probeFunc(func(context.Context, string) error { return nil }), Platform: plat, Snaps: current,
		Conn: Connection{Ref: v1alpha1.ObjectRef{Namespace: "sre", Name: "slack"}}, Metrics: m, Log: slog.New(slog.DiscardHandler),
		Interval: time.Hour, Tick: 5 * time.Millisecond, Revision: func(context.Context) string { return "r" },
	}.newProber()
	got := settled(t, p, 2)

	c := got["sre/with"][bindingcheck.ApproversReadable]
	if c.Status != bindingcheck.False || c.Reason != bindingcheck.ReasonGroupNotFound || !strings.Contains(c.Message, "approverGroups[1]") {
		t.Errorf("ApproversReadable of the Binding with groups = %s/%s %q; want False/GroupNotFound naming approverGroups[1]", c.Status, c.Reason, c.Message)
	}
	if r := got["sre/with"][bindingcheck.Ready]; r.Status != bindingcheck.False || r.Reason != "ApproversReadable:GroupNotFound" {
		t.Errorf("Ready = %s/%s; want False/ApproversReadable:GroupNotFound", r.Status, r.Reason)
	}
	if _, has := got["sre/without"][bindingcheck.ApproversReadable]; has {
		t.Error("a Binding with no groups has an ApproversReadable condition")
	}
	if r := got["sre/without"][bindingcheck.Ready]; r.Status != bindingcheck.True {
		t.Errorf("Ready of the Binding with no groups = %s; want True", r.Status)
	}
	if len(plat.groupsCalls) != 1 || !reflect.DeepEqual(plat.groupsCalls[0], approverGroups) {
		t.Errorf("platform calls = %v; want one, over the three groups of the one Binding that has them", plat.groupsCalls)
	}
	if _, n := meanProbeSeconds(t, m, bindingcheck.ApproversReadable); n != 1 {
		t.Errorf("%d observations in the ApproversReadable bucket of the probe-duration histogram; want 1", n)
	}
	if series := strings.Join(seriesOf(t, m, "agentkourier_binding_condition"), "\n"); !strings.Contains(series, "binding=sre/with,condition=ApproversReadable,status=false} 1") {
		t.Errorf("series:\n%s\nwant the ApproversReadable series of sre/with, false", series)
	}
}

// The Targets carry a Binding's approver groups only when it asks for tool approvals from groups; a Binding that asks for approvals
// from anyone, or for none, carries none, and the Targets do not share the Binding's slice.
func TestBindingTargetsCarryTheApproverGroupsOfABindingThatAsksForThem(t *testing.T) {
	groups := map[string][]string{"sre/two": {"S0AAAAAAAAA", "S0BBBBBBBBB"}}
	snap := snapshotWith(t, map[string]int64{"sre/two": 1, "sre/none": 1}, nil, groups)
	got := bindingTargets(snap, v1alpha1.ObjectRef{Namespace: "sre", Name: "slack"})
	if len(got) != 2 {
		t.Fatalf("targets = %+v", got)
	}
	for _, tg := range got {
		want := groups[tg.Binding]
		if !reflect.DeepEqual(tg.ApproverGroups, want) {
			t.Errorf("%s: ApproverGroups = %v, want %v", tg.Binding, tg.ApproverGroups, want)
		}
	}
	for _, rb := range snap.Bindings() {
		if len(rb.Binding.Spec.Interactions.ApproverGroups) > 0 {
			for _, tg := range got {
				if len(tg.ApproverGroups) > 0 {
					tg.ApproverGroups[0] = "SMUTATED000"
				}
			}
			if rb.Binding.Spec.Interactions.ApproverGroups[0] == "SMUTATED000" {
				t.Error("a Target shares the Binding's slice of groups")
			}
		}
	}
}

func TestAskingForApproversNeedsToolApprovalsAndGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   v1alpha1.Interactions
		want bool
	}{
		{"nothing", v1alpha1.Interactions{}, false},
		{"approvals from anyone", v1alpha1.Interactions{ToolApprovals: true}, false},
		{"approvals from a group", v1alpha1.Interactions{ToolApprovals: true, ApproverGroups: []string{"S0AAAAAAAAA"}}, true},
		{"groups without approvals (the API refuses it)", v1alpha1.Interactions{ApproverGroups: []string{"S0AAAAAAAAA"}}, false},
	} {
		if got := asksForApprovers(v1alpha1.BindingSpec{Interactions: tc.in}); got != tc.want {
			t.Errorf("%s: asksForApprovers = %t, want %t", tc.name, got, tc.want)
		}
	}
}
