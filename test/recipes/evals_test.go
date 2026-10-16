package recipes_test

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// triggersFile is a recipe's trigger evals, relative to the recipe.
var triggersFile = filepath.Join("evals", "triggers.yaml")

// triggerEvals is the content of a recipe's evals/triggers.yaml: messages, and what the recipe's Binding
// must make of each.
type triggerEvals struct {
	Cases []triggerCase `json:"cases"`
}

// triggerCase is one message and the outcome expected of it.
type triggerCase struct {
	// Name identifies the case in a report. Unique in the file.
	Name string `json:"name"`
	// Binding is "namespace/name" of a Binding in the recipe's kourier/ directory.
	Binding string       `json:"binding"`
	Message evalMessage  `json:"message"`
	Expect  evalExpected `json:"expect"`
}

// evalMessage is a message as the adapter hands it to the intake. A message with a Bot is a bot's; without
// one it is a person's, and then only User and Text are read.
type evalMessage struct {
	// Bot is the poster's bot ID, never its display name.
	Bot  string `json:"bot,omitempty"`
	User string `json:"user,omitempty"`
	// Text is the message's own text. The other fields are those of a bot's first attachment, as the adapter
	// flattens it (v1alpha1.MessageField).
	Text           string `json:"text,omitempty"`
	Title          string `json:"title,omitempty"`
	TitleLink      string `json:"titleLink,omitempty"`
	Fallback       string `json:"fallback,omitempty"`
	Footer         string `json:"footer,omitempty"`
	AttachmentText string `json:"attachmentText,omitempty"`
	// InThread says the message is a reply inside a thread, not a top-level post.
	InThread bool `json:"inThread,omitempty"`
}

// evalExpected is the verdict a case requires.
type evalExpected struct {
	// Outcome is a chat.ReplayOutcome.
	Outcome string `json:"outcome"`
	// Trigger, for a matched message, is the index in the Binding's chat.triggers of the trigger that takes
	// it. Unchecked when omitted.
	Trigger *int `json:"trigger,omitempty"`
	// Fields, for a matched message, are the values the trigger extracts. They must equal the extracted
	// values exactly. Unchecked when omitted.
	Fields map[string]string `json:"fields,omitempty"`
}

// evalOutcomes are the outcomes a case may expect. "own" is left out: it depends on a flag only the adapter
// can set, so no message can produce it.
var evalOutcomes = []chat.ReplayOutcome{chat.ReplayMatched, chat.ReplayUnmatched, chat.ReplayExcluded,
	chat.ReplayOtherBot, chat.ReplayPerson, chat.ReplayIgnored}

// parseTriggerEvals decodes and validates the text of a triggers.yaml. Unknown keys, duplicate keys and
// inconsistent cases are errors, all reported together.
func parseTriggerEvals(data []byte) (*triggerEvals, error) {
	var evals triggerEvals
	if err := config.DecodeStrict(data, &evals); err != nil {
		return nil, err
	}
	var errs []error
	if len(evals.Cases) == 0 {
		errs = append(errs, errors.New("cases: at least one case is required"))
	}
	seen := map[string]bool{}
	for i, c := range evals.Cases {
		if c.Name == "" {
			errs = append(errs, fmt.Errorf("cases[%d]: name is required", i))
		} else if seen[c.Name] {
			errs = append(errs, fmt.Errorf("cases[%d]: name %q is used by an earlier case", i, c.Name))
		}
		seen[c.Name] = true
		for _, p := range c.problems() {
			errs = append(errs, fmt.Errorf("case %q: %s", c.Name, p))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &evals, nil
}

// problems lists what is inconsistent in the case itself, without the Binding it names.
func (c triggerCase) problems() []string {
	var out []string
	if ns, name, ok := strings.Cut(c.Binding, "/"); !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		out = append(out, fmt.Sprintf("binding %q must be namespace/name", c.Binding))
	}
	m := c.Message
	if m.Bot == "" && (m.Title != "" || m.TitleLink != "" || m.Fallback != "" || m.Footer != "" || m.AttachmentText != "") {
		out = append(out, "a person's message (no bot) has only user and text; title, titleLink, fallback, footer and attachmentText belong to a bot's")
	}
	if m.Bot != "" && m.Text == "" && m.Title == "" && m.TitleLink == "" && m.Fallback == "" && m.Footer == "" && m.AttachmentText == "" {
		out = append(out, "a bot's message needs text or an attachment field; one with neither is dropped before any trigger sees it")
	}
	if m.Bot == "" && (m.User == "" || m.Text == "") {
		out = append(out, "a person's message needs user and text; one without either is dropped before any trigger sees it")
	}
	if !slices.Contains(evalOutcomes, chat.ReplayOutcome(c.Expect.Outcome)) {
		out = append(out, fmt.Sprintf("expect.outcome %q must be one of %v", c.Expect.Outcome, evalOutcomes))
	}
	if c.Expect.Outcome != string(chat.ReplayMatched) && (c.Expect.Trigger != nil || c.Expect.Fields != nil) {
		out = append(out, "expect.trigger and expect.fields apply only to a matched message")
	}
	return out
}

// history builds the message as the adapter hands it on, in the channel of b, with slack.InboundOf, which the
// live path and the dry run's history both use. The connection is named as the dry run names it. The dry run's
// own reading of a Slack payload (Client.History) needs a Slack API to call, so the Message it would have read
// is made here instead.
func (m evalMessage) history(b *v1alpha1.Binding) chat.HistoryMessage {
	conn := v1alpha1.NamespacedName(cmp.Or(b.Spec.Chat.ConnectionRef.Namespace, b.Namespace), b.Spec.Chat.ConnectionRef.Name)
	msg := slack.Message{Channel: b.Spec.Chat.Channel, TS: "1.1", User: m.User, Text: m.Text}
	if m.InThread {
		msg.ThreadTS = "1.0"
	}
	if m.Bot != "" {
		msg.Bot = slack.BotPost{ID: m.Bot, Title: m.Title, TitleLink: m.TitleLink, Fallback: m.Fallback,
			Footer: m.Footer, Text: m.AttachmentText}
	}
	return chat.HistoryMessage{Message: slack.InboundOf(conn, msg)}
}

// checkTriggerEvals runs the trigger evals of the recipe at dir, if it has any, through chat.Replay (the dry
// run's engine, which uses the intake's own matcher) over the Bindings of set, the recipe's loaded kourier/
// directory. It reports every mismatch, naming the recipe, the case, what was expected and got, and the
// verdict's reason. It returns ran false, with no error, for a recipe without evals/triggers.yaml.
func checkTriggerEvals(dir string, set *config.Set) (ran bool, err error) {
	file := filepath.Join(dir, triggersFile)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("read %s: %w", file, err)
	}
	evals, err := parseTriggerEvals(data)
	if err != nil {
		return true, underFile(file, err)
	}
	replays := map[string]*chat.Replay{}
	var errs []error
	for _, c := range evals.Cases {
		i := slices.IndexFunc(set.Bindings, func(b v1alpha1.Binding) bool { return b.Namespace+"/"+b.Name == c.Binding })
		if i < 0 {
			errs = append(errs, fmt.Errorf("case %q: Binding %s is not in the recipe's kourier directory", c.Name, c.Binding))
			continue
		}
		b := &set.Bindings[i]
		r := replays[c.Binding]
		if r == nil {
			if r, err = chat.NewReplay(b); err != nil {
				errs = append(errs, fmt.Errorf("case %q: Binding %s: %w", c.Name, c.Binding, err))
				continue
			}
			replays[c.Binding] = r
		}
		if msg := c.mismatch(r.Classify(c.Message.history(b))); msg != "" {
			errs = append(errs, fmt.Errorf("case %q (Binding %s): %s", c.Name, c.Binding, msg))
		}
	}
	return true, underFile(file, errors.Join(errs...))
}

// underFile prefixes each problem of err with the file, so that the report says which recipe's evals failed.
func underFile(file string, err error) error {
	if err == nil {
		return nil
	}
	return mapEach(err, func(e error) error { return fmt.Errorf("%s: %w", file, e) })
}

// mismatch describes how v differs from what the case expects, or returns "" when it does not.
func (c triggerCase) mismatch(v chat.ReplayVerdict) string {
	var diffs []string
	if string(v.Outcome) != c.Expect.Outcome {
		diffs = append(diffs, fmt.Sprintf("outcome: want %s, got %s", c.Expect.Outcome, v.Outcome))
	} else if v.Outcome == chat.ReplayMatched {
		if want := c.Expect.Trigger; want != nil && *want != v.Trigger {
			diffs = append(diffs, fmt.Sprintf("trigger: want %d, got %d", *want, v.Trigger))
		}
		if want := c.Expect.Fields; want != nil && (len(want) != 0 || len(v.Fields) != 0) && !reflect.DeepEqual(want, v.Fields) {
			diffs = append(diffs, fmt.Sprintf("fields: want %v, got %v", want, v.Fields))
		}
	}
	if len(diffs) == 0 {
		return ""
	}
	if v.Why != "" {
		diffs = append(diffs, "why: "+v.Why)
	}
	return strings.Join(diffs, "; ")
}

// TestTriggerEvals runs the trigger evals of every fixture recipe: the one that passes, and the ones that must
// fail, with the case or the key named.
func TestTriggerEvals(t *testing.T) {
	tests := []struct {
		name    string
		want    []string // substrings of the error; none means the evals must pass
		wantRan bool
	}{
		{name: "evals-pass", wantRan: true},
		{name: "evals-wrong-regex", wantRan: true, want: []string{
			"evals-wrong-regex/evals/triggers.yaml", `case "firing alert" (Binding recipe/alerts)`,
			"outcome: want matched, got unmatched", "why: trigger 1: match titleLink",
		}},
		{name: "evals-unknown-key", wantRan: true, want: []string{"evals-unknown-key/evals/triggers.yaml", "cases[0].expect.triger: unknown field"}},
		{name: "valid"}, // a recipe without evals/triggers.yaml
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran, err := checkRecipe(t.Context(), filepath.Join("testdata", tt.name))
			if ran != tt.wantRan {
				t.Errorf("ran = %v, want %v", ran, tt.wantRan)
			}
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("checkRecipe: %v; want it to pass", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkRecipe passed; want an error containing %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(filepath.ToSlash(err.Error()), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// TestParseTriggerEvals pins what a triggers.yaml may hold.
func TestParseTriggerEvals(t *testing.T) {
	const ok = `
cases:
  - name: a
    binding: ns/b
    message: {bot: B1, title: t}
    expect: {outcome: matched, trigger: 1, fields: {k: v}}
`
	tests := []struct {
		name, in string
		want     string // substring of the error; empty means it parses
	}{
		{"valid", ok, ""},
		{"unknown key at the top", ok + "extra: 1\n", "extra: unknown field"},
		{"unknown message key", strings.Replace(ok, "title: t", "titel: t", 1), "cases[0].message.titel: unknown field"},
		{"a duplicate key", strings.Replace(ok, "binding: ns/b", "binding: ns/b\n    binding: ns/c", 1), "already set"},
		{"no cases", "cases: []\n", "at least one case"},
		{"no name", strings.Replace(ok, "name: a", "name: ''", 1), "name is required"},
		{"a repeated name", ok + strings.TrimPrefix(ok, "\ncases:\n"), "used by an earlier case"},
		{"binding without a namespace", strings.Replace(ok, "ns/b", "b", 1), "must be namespace/name"},
		{"unknown outcome", strings.Replace(ok, "outcome: matched", "outcome: mached", 1), `expect.outcome "mached" must be one of`},
		{"own is not producible", strings.Replace(ok, "outcome: matched", "outcome: own", 1), `expect.outcome "own"`},
		{"fields on an unmatched message", strings.Replace(ok, "outcome: matched, trigger: 1,", "outcome: unmatched,", 1), "apply only to a matched message"},
		{"a person with a title", strings.Replace(ok, "{bot: B1, title: t}", "{user: U1, title: t}", 1), "a person's message"},
		{"a bot with nothing to read", strings.Replace(ok, "{bot: B1, title: t}", "{bot: B1, inThread: true}", 1), "dropped before any trigger sees it"},
		{"a bot with text only", strings.Replace(ok, "{bot: B1, title: t}", "{bot: B1, text: hi}", 1), ""},
		{"a person with text", strings.Replace(ok, "{bot: B1, title: t}", "{user: U1, text: hi}", 1), ""},
		{"a person with no text", strings.Replace(ok, "{bot: B1, title: t}", "{user: U1}", 1), "a person's message needs user and text"},
		{"a person with no user", strings.Replace(ok, "{bot: B1, title: t}", "{text: hi}", 1), "a person's message needs user and text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTriggerEvals([]byte(tt.in))
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("parseTriggerEvals: %v; want it to parse", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("parseTriggerEvals error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// TestTriggerEvalsMismatchReports pins how a mismatch reads, for each way a case can differ.
func TestTriggerEvalsMismatchReports(t *testing.T) {
	one := 1
	c := triggerCase{Expect: evalExpected{Outcome: "matched", Trigger: &one, Fields: map[string]string{"a": "1"}}}
	tests := []struct {
		name string
		v    chat.ReplayVerdict
		want string
	}{
		{"as expected", chat.ReplayVerdict{Outcome: chat.ReplayMatched, Trigger: 1, Fields: map[string]string{"a": "1"}}, ""},
		{"another outcome, with why", chat.ReplayVerdict{Outcome: chat.ReplayExcluded, Why: "trigger 1: exclude title"}, "outcome: want matched, got excluded; why: trigger 1: exclude title"},
		{"another trigger", chat.ReplayVerdict{Outcome: chat.ReplayMatched, Trigger: 2, Fields: map[string]string{"a": "1"}}, "trigger: want 1, got 2"},
		{"other fields", chat.ReplayVerdict{Outcome: chat.ReplayMatched, Trigger: 1, Fields: map[string]string{"a": "2"}}, "fields: want map[a:1], got map[a:2]"},
		{"an extra field", chat.ReplayVerdict{Outcome: chat.ReplayMatched, Trigger: 1, Fields: map[string]string{"a": "1", "b": "2"}}, "fields: want"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.mismatch(tt.v); !strings.Contains(got, tt.want) || (tt.want == "") != (got == "") {
				t.Errorf("mismatch = %q, want %q", got, tt.want)
			}
		})
	}
}
