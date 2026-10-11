package slack

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// slackError is an error as the Client returns Slack's: the code, wrapped.
func slackError(code string) error {
	return fmt.Errorf("slack chat.postMessage: %w", slackgo.SlackErrorResponse{Err: code})
}

var errTimeout = errors.New("dial tcp: i/o timeout")

func in(typ string, values ...string) Input { return Input{Type: typ, Values: values} }

// The state of a prompt's controls becomes the answer of each question, by the block and action IDs that
// renderPrompt made, and each only as the control it was asked in.
func TestAnswersAreReadByQuestionAndOnlyAsTheControlTheyWereAskedIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inputs map[string]map[string]Input
		want   []chat.Answer
	}{
		{"free text", map[string]map[string]Input{"q0": {"t0": in("plain_text_input", "prod")}}, []chat.Answer{{Question: 0, Values: []string{"prod"}}}},
		{"one choice, by its index", map[string]map[string]Input{"q1": {"r1": in("radio_buttons", "2")}}, []chat.Answer{{Question: 1, Values: []string{"2"}}}},
		{"several choices", map[string]map[string]Input{"q0": {"c0": in("checkboxes", "0", "2")}}, []chat.Answer{{Question: 0, Values: []string{"0", "2"}}}},
		{"a select", map[string]map[string]Input{"q0": {"s0": in("static_select", "7")}}, []chat.Answer{{Question: 0, Values: []string{"7"}}}},
		{"a multi select", map[string]map[string]Input{"q0": {"m0": in("multi_static_select", "1", "5")}}, []chat.Answer{{Question: 0, Values: []string{"1", "5"}}}},
		{"numbers typed beside a list are 1-based positions, and become indices",
			map[string]map[string]Input{"q0": {"n0": in("plain_text_input", "1, 3 ;4 2")}}, []chat.Answer{{Question: 0, Values: []string{"0", "2", "3", "1"}}}},
		{"a token that is no number stays as typed, to be found no choice",
			map[string]map[string]Input{"q0": {"n0": in("plain_text_input", "2, abc, 0, -1")}}, []chat.Answer{{Question: 0, Values: []string{"1", "abc", "0", "-1"}}}},
		{"answers in order of question",
			map[string]map[string]Input{"q2": {"t2": in("plain_text_input", "c")}, "q0": {"t0": in("plain_text_input", "a")}, "q1": {"r1": in("radio_buttons", "0")}},
			[]chat.Answer{{Question: 0, Values: []string{"a"}}, {Question: 1, Values: []string{"0"}}, {Question: 2, Values: []string{"c"}}}},

		{"an element of another type than its action ID says is no answer", map[string]map[string]Input{"q0": {"r0": in("checkboxes", "0", "1")}}, nil},
		{"a text input where a select was asked is no answer", map[string]map[string]Input{"q0": {"s0": in("plain_text_input", "7")}}, nil},
		{"a select where text was asked is no answer", map[string]map[string]Input{"q0": {"t0": in("static_select", "7")}}, nil},
		{"an action ID of another question is no answer", map[string]map[string]Input{"q0": {"t1": in("plain_text_input", "x")}}, nil},
		{"an action ID with no control letter is no answer", map[string]map[string]Input{"q0": {"x0": in("plain_text_input", "x"), "0": in("plain_text_input", "x")}}, nil},
		{"an input that holds nothing is no answer", map[string]map[string]Input{"q0": {"t0": in("plain_text_input")}}, nil},
		{"a block that is no question's is ignored", map[string]map[string]Input{"submit": {"t0": in("plain_text_input", "x")}, "q": {"t0": in("plain_text_input", "x")}, "q00000": {"t0": in("plain_text_input", "x")}, "Q0": {"t0": in("plain_text_input", "x")}}, nil},
		{"no inputs", nil, nil},
	} {
		got := answers(tc.inputs)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: answers = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Platform.interaction is the decode Run hands the sink: where the message is, who pressed,
// the button's value and the answers of the controls.
func TestPlatformInteractionDecodesAPress(t *testing.T) {
	got := NewPlatform(nil, "ns/conn").interaction(Interaction{User: "U1", Channel: "C1", ThreadTS: "1.1", MessageTS: "1.2", ActionID: "submit", BlockID: "submit", Value: "ix-1",
		Inputs: map[string]map[string]Input{"q0": {"r0": in("radio_buttons", "1")}}})
	want := chat.Interaction{User: "U1", PromptID: "ix-1", At: chat.Ref{Platform: "slack", Conn: "ns/conn", Channel: "C1", Thread: "1.1", ID: "1.2"},
		Answers: []chat.Answer{{Question: 0, Values: []string{"1"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Interaction = %+v, want %+v", got, want)
	}
}

func TestClassifyMapsTheCodesToNeutralReasonsAndKeepsTheRawCode(t *testing.T) {
	p := NewPlatform(nil, "ns/conn")
	for _, tc := range []struct {
		name string
		op   chat.Op
		err  error
		want chat.Verdict
	}{
		{"a channel that is gone", chat.OpPost, slackError("channel_not_found"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "channel_not_found"}},
		{"an archived channel", chat.OpPost, slackError("is_archived"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "is_archived"}},
		{"Agent Kourier not in the channel", chat.OpPost, slackError("not_in_channel"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "not_in_channel"}},
		{"text too long", chat.OpPost, slackError("msg_too_long"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonTooLong, Code: "msg_too_long"}},
		{"blocks Slack refuses", chat.OpPost, slackError("invalid_blocks"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: "invalid_blocks"}},
		{"blocks in a form Slack refuses", chat.OpPost, slackError("invalid_blocks_format"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: "invalid_blocks_format"}},
		{"a reaction on a message that is gone", chat.OpReact, slackError("message_not_found"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "message_not_found"}},
		{"a reaction that is no emoji", chat.OpReact, slackError("invalid_name"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: "invalid_name"}},
		{"a thread that is locked", chat.OpReact, slackError("thread_locked"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: "thread_locked"}},
		{"a limit on reactions", chat.OpReact, slackError("too_many_reactions"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: "too_many_reactions"}},
		{"a rejected token", chat.OpPost, slackError("invalid_auth"), chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}},
		{"a revoked token on a reaction", chat.OpReact, slackError("token_revoked"), chat.Verdict{Class: chat.FatalAuth, Code: "token_revoked"}},
		{"an inactive account", chat.OpOther, slackError("account_inactive"), chat.Verdict{Class: chat.FatalAuth, Code: "account_inactive"}},
		{"a rate limit passes", chat.OpPost, slackError("ratelimited"), chat.Verdict{Class: chat.Transient}},
		{"a rate limit says how long to wait", chat.OpPost, fmt.Errorf("slack chat.update: %w", &slackgo.RateLimitedError{RetryAfter: 7 * time.Second}), chat.Verdict{Class: chat.Transient, RetryAfter: 7 * time.Second}},
		{"a rate limit that names no wait is one second", chat.OpPost, fmt.Errorf("slack chat.update: %w", &slackgo.RateLimitedError{}), chat.Verdict{Class: chat.Transient, RetryAfter: time.Second}},
		{"a server error passes", chat.OpPost, slackError("internal_error"), chat.Verdict{Class: chat.Transient}},
		{"a post's permanent code is not a reaction's", chat.OpReact, slackError("msg_too_long"), chat.Verdict{Class: chat.Transient}},
		{"a reaction's permanent code is not a post's", chat.OpPost, slackError("thread_locked"), chat.Verdict{Class: chat.Transient}},
		{"another operation has no permanent code", chat.OpOther, slackError("channel_not_found"), chat.Verdict{Class: chat.Transient}},
		{"an error that is not Slack's", chat.OpPost, errTimeout, chat.Verdict{Class: chat.Transient}},
		{"no error", chat.OpPost, nil, chat.Verdict{Class: chat.Transient}},
	} {
		if got := p.Classify(tc.op, tc.err); got != tc.want {
			t.Errorf("%s: Classify = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

type keep struct{ got []chat.Inbound }

func (k *keep) Message(_ context.Context, m chat.Inbound) error     { k.got = append(k.got, m); return nil }
func (k *keep) Interaction(context.Context, chat.Interaction) error { return nil }

// A message in a conversation of its own with Agent Kourier is Direct, which the routing reads in place of a
// mention. The Client drops direct messages until Phase 4, so the mapping is tested here and not through
// the fake Slack.
func TestAMessageInAnIMIsDirect(t *testing.T) {
	k := &keep{}
	sink := inbound{p: NewPlatform(nil, "ns/conn"), sink: k}
	for _, m := range []Message{{Channel: "D1", TS: "1.1", User: "U1", Text: "hi", ChannelType: "im"}, {Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: "U1", Text: "hi", ChannelType: "channel"}} {
		if err := sink.Message(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if !k.got[0].Direct || k.got[1].Direct || k.got[1].At.Thread != "1.0" || k.got[0].At.Channel != "D1" {
		t.Errorf("got %+v, want the first direct and the second a threaded channel message", k.got)
	}
}
