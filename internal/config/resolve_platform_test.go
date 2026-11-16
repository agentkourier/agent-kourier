package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// onSlack is the message of a chat ID of sre's Binding that is not Slack's: the problem and the fix that PlatformMismatches gives,
// with the platform, the connection and the page.
func onSlack(path, problem, fix string) string {
	return path + ": " + problem + " on Slack, the platform of ChatConnection agent-kourier-system/slack-main: " + fix +
		" (see " + PlatformsPage + ")"
}

const (
	channelFix = "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"
	botFix     = "use the bot's ID, such as B0123456789, not its name"
)

// TestPlatformIDs holds Resolve and ResolvePartial to the shapes of the connection's platform: a Binding of a Slack connection
// whose chat IDs have another platform's shape, which the load takes, is rejected with InvalidPlatformID, one rejection for each
// ID in the order of the fields, and holds no channel.
func TestPlatformIDs(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Set)
		wantServed []string
		rejected   []Rejection // of sre-alerts
	}{
		{"Slack's own shapes", func(*Set) {}, []string{"sre/chat-only", "sre/sre-alerts"}, nil},
		{"a repository as the channel", func(s *Set) { sreAlerts(s).Spec.Chat.Channel = "octo-org/hello-world" }, []string{"sre/chat-only"},
			[]Rejection{rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.channel", `"octo-org/hello-world" is not a channel ID`, channelFix))}},
		{"a bot's name as the sender, which has a login's shape", func(s *Set) { sreAlerts(s).Spec.Chat.Triggers[1].From.ID = "alertmanager" }, []string{"sre/chat-only"},
			[]Rejection{rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.triggers[1].from.id", `"alertmanager" is not a bot ID`, botFix))}},
		{"every ID, in the order of the fields", func(s *Set) {
			b := sreAlerts(s)
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Triggers[1].From.ID = "dependabot[bot]"
			b.Spec.Chat.Mentions = []v1alpha1.ChatMention{{Name: "oct", User: "octocat"}}
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"octo-org/sre"}
		}, []string{"sre/chat-only"}, []Rejection{
			rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.channel", `"octo-org/hello-world" is not a channel ID`, channelFix)),
			rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.triggers[1].from.id", `"dependabot[bot]" is not a bot ID`, botFix)),
			rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.mentions[0].user", `"octocat" is not a user ID`, "use the person's user ID, such as U0123456789")),
			rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.interactions.approverGroups[0]", `"octo-org/sre" is not a user group ID`, "use the user group's ID, such as S0123456789")),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("ResolvePartial", func(t *testing.T) {
				set := specSet(t)
				tc.mutate(set)
				snap, got := ResolvePartial(set)
				if !slices.Equal(got, tc.rejected) {
					t.Errorf("rejections:\n%+v\nwant:\n%+v", got, tc.rejected)
				}
				if s := served(snap); !slices.Equal(s, tc.wantServed) {
					t.Errorf("served = %v; want %v", s, tc.wantServed)
				}
			})
			t.Run("Resolve", func(t *testing.T) {
				set := specSet(t)
				tc.mutate(set)
				if len(tc.rejected) == 0 {
					if _, err := Resolve(t.Context(), set, specSecrets()); err != nil {
						t.Fatalf("Resolve: %v", err)
					}
					return
				}
				var want []string
				for _, r := range tc.rejected {
					want = append(want, atSREAlerts+"Binding sre/sre-alerts: "+r.Message)
				}
				wantProblems(t, set, specSecrets(), want...)
			})
		})
	}
}

// A Binding rejected for its platform holds no channel, so that a younger Binding of the same channel is served.
func TestPlatformRejectedBindingHoldsNoChannel(t *testing.T) {
	set := specSet(t)
	sreAlerts(set).CreationTimestamp = at(2)
	set.Bindings = append(set.Bindings, bindingLike(set, "sre", "older", "C0123456789", func(b *v1alpha1.Binding) {
		b.CreationTimestamp = at(1)
		b.Spec.Chat.Mentions = []v1alpha1.ChatMention{{Name: "oct", User: "octocat"}}
	}))
	snap, got := ResolvePartial(set)
	want := []Rejection{rejected("older", ReasonInvalidPlatformID, onSlack("spec.chat.mentions[0].user", `"octocat" is not a user ID`, "use the person's user ID, such as U0123456789"))}
	if !slices.Equal(got, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
	}
	if s := served(snap); !slices.Equal(s, []string{"sre/chat-only", "sre/sre-alerts"}) {
		t.Errorf("served = %v; want chat-only and sre-alerts", s)
	}
}

// A Binding that the connection does not admit learns nothing of the connection's platform: it is told NamespaceNotAllowed and
// no more, whatever its IDs.
func TestPlatformIsNotToldToABindingTheConnectionDoesNotAdmit(t *testing.T) {
	set := specSet(t)
	set.ChatConnections[0].Spec.AllowedNamespaces = []string{"payments"}
	sreAlerts(set).Spec.Chat.Channel = "octo-org/hello-world"
	_, got := ResolvePartial(set)
	for _, r := range got {
		if r.Reason != ReasonNamespaceNotAllowed || strings.Contains(r.Message, "Slack") {
			t.Errorf("rejection = %+v; want NamespaceNotAllowed and no word of the platform", r)
		}
	}
	if len(got) != 2 {
		t.Errorf("rejections = %+v; want one NamespaceNotAllowed for each Binding", got)
	}
}

// A platform that cannot serve a feature the Binding asks for rejects it with UnsupportedOnPlatform. Slack serves every feature
// that the rules name, so the case is made with a table of the test's own: Slack's, without tool approvals.
func TestUnsupportedOnPlatform(t *testing.T) {
	noApprovals := func(spec v1alpha1.ChatConnectionSpec) (v1alpha1.ChatPlatform, bool) {
		p, ok := v1alpha1.ChatPlatformOf(spec)
		p.ToolApprovals = false
		return p, ok
	}
	const msg = "spec.interactions.toolApprovals: tool approvals are not supported on Slack, the platform of ChatConnection " +
		"agent-kourier-system/slack-main: set it to false (see " + PlatformsPage + ")"
	asks := func(set *Set) { sreAlerts(set).Spec.Interactions.ToolApprovals = true }

	t.Run("ResolvePartial", func(t *testing.T) {
		set := specSet(t)
		asks(set)
		snap, got := resolvePartial(set, noApprovals)
		if want := []Rejection{rejected("sre-alerts", ReasonUnsupportedOnPlatform, msg)}; !slices.Equal(got, want) {
			t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
		}
		if s := served(snap); !slices.Equal(s, []string{"sre/chat-only"}) {
			t.Errorf("served = %v; want chat-only, which asks for no tool approvals", s)
		}
	})
	t.Run("Resolve", func(t *testing.T) {
		set := specSet(t)
		asks(set)
		_, err := resolve(t.Context(), set, specSecrets(), noApprovals)
		if err == nil || !strings.Contains(err.Error(), atSREAlerts+"Binding sre/sre-alerts: "+msg) {
			t.Errorf("Resolve = %v; want the problem %q", err, msg)
		}
	})
	t.Run("a Binding that asks for none", func(t *testing.T) {
		if _, got := resolvePartial(specSet(t), noApprovals); len(got) != 0 {
			t.Errorf("rejections = %+v; want none", got)
		}
	})
}

// RefusedOnPlatform is the check of resolution, given to a source that fails one object at a time: the reason of the first ID or
// feature the platform does not take, and every one of them in the words of resolution, joined; nothing for a Binding that the
// platform takes, or that the connection does not admit, which learns nothing of the platform.
func TestRefusedOnPlatform(t *testing.T) {
	noApprovals := func(spec v1alpha1.ChatConnectionSpec) (v1alpha1.ChatPlatform, bool) {
		p, ok := v1alpha1.ChatPlatformOf(spec)
		p.ToolApprovals = false
		return p, ok
	}
	channel := onSlack("spec.chat.channel", `"octo-org/hello-world" is not a channel ID`, channelFix)
	sender := onSlack("spec.chat.triggers[1].from.id", `"alertmanager" is not a bot ID`, botFix)
	approvals := "spec.interactions.toolApprovals: tool approvals are not supported on Slack, the platform of ChatConnection " +
		"agent-kourier-system/slack-main: set it to false (see " + PlatformsPage + ")"
	tests := []struct {
		name        string
		mutate      func(*Set)
		platforms   platformRules
		wantReason  RejectionReason
		wantMessage string
	}{
		{"Slack's own shapes", func(*Set) {}, v1alpha1.ChatPlatformOf, "", ""},
		{"a bot's name as the sender", func(s *Set) { sreAlerts(s).Spec.Chat.Triggers[1].From.ID = "alertmanager" }, v1alpha1.ChatPlatformOf,
			ReasonInvalidPlatformID, sender},
		{"two IDs, joined in the order of the fields", func(s *Set) {
			sreAlerts(s).Spec.Chat.Channel = "octo-org/hello-world"
			sreAlerts(s).Spec.Chat.Triggers[1].From.ID = "alertmanager"
		}, v1alpha1.ChatPlatformOf, ReasonInvalidPlatformID, channel + "; " + sender},
		{"a feature the platform does not serve", func(s *Set) { sreAlerts(s).Spec.Interactions.ToolApprovals = true }, noApprovals,
			ReasonUnsupportedOnPlatform, approvals},
		{"a Binding that the connection does not admit", func(s *Set) {
			s.ChatConnections[0].Spec.AllowedNamespaces = []string{"payments"}
			sreAlerts(s).Spec.Chat.Triggers[1].From.ID = "alertmanager"
		}, v1alpha1.ChatPlatformOf, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := specSet(t)
			tc.mutate(set)
			reason, message := refusedOnPlatform(sreAlerts(set), &set.ChatConnections[0], tc.platforms)
			if reason != tc.wantReason || message != tc.wantMessage {
				t.Errorf("refusedOnPlatform = %q, %q; want %q, %q", reason, message, tc.wantReason, tc.wantMessage)
			}
		})
	}
	set := specSet(t)
	sreAlerts(set).Spec.Chat.Triggers[1].From.ID = "alertmanager"
	if reason, message := RefusedOnPlatform(sreAlerts(set), &set.ChatConnections[0]); reason != ReasonInvalidPlatformID || message != sender {
		t.Errorf("RefusedOnPlatform = %q, %q; want the rules of v1alpha1.ChatPlatformOf", reason, message)
	}
}
