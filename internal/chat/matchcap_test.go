package chat_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// fieldCap is the bytes of a field that a trigger's regexes see.
const fieldCap = 16 * 1024

// withField returns a bot message whose field f is v and whose other fields are empty.
func withField(f v1alpha1.MessageField, v string) chat.Inbound {
	b := chat.BotMessage{ID: webhookBot}
	m := botInbound("C1", "1.1", b)
	switch f {
	case v1alpha1.FieldText:
		m.Text = v
	case v1alpha1.FieldTitle:
		m.Bot.Title = v
	case v1alpha1.FieldLink:
		m.Bot.Link = v
	case v1alpha1.FieldFallback:
		m.Bot.Fallback = v
	case v1alpha1.FieldFooter:
		m.Bot.Footer = v
	case v1alpha1.FieldBody:
		m.Bot.Body = v
	}
	return m
}

var allFields = []v1alpha1.MessageField{v1alpha1.FieldText, v1alpha1.FieldTitle, v1alpha1.FieldLink, v1alpha1.FieldFallback, v1alpha1.FieldFooter, v1alpha1.FieldBody}

// classify runs the message through the trigger by the intake's own matcher, as the dry run does.
func classify(t *testing.T, tr v1alpha1.Trigger, m chat.Inbound) chat.ReplayVerdict {
	t.Helper()
	return replayOf(t, newTriggered(t, tr)).Classify(chat.HistoryMessage{Message: m})
}

// Only the first 16 KiB of a field is matched, excluded on and extracted from, for every field a regex can name: a
// regex finds what is in the head of a longer field, and finds nothing that is past it.
func TestOnlyTheHeadOfAFieldIsMatched(t *testing.T) {
	head := "HEADMARK" + strings.Repeat("x", fieldCap-len("HEADMARK")-len("TAIL")) + "TAIL" // exactly the cap
	long := head + "TAILMARK" + strings.Repeat("y", 1000)                                   // past it, with a mark that only a regex that reads on finds
	for _, f := range allFields {
		t.Run(string(f), func(t *testing.T) {
			matching := func(re string) v1alpha1.Trigger {
				return botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Match[f] = re })
			}
			for _, tc := range []struct {
				name string
				tr   v1alpha1.Trigger
				msg  chat.Inbound
				want chat.ReplayOutcome
			}{
				{"a match on the head of a long field", matching(`HEADMARK`), withField(f, long), chat.ReplayMatched},
				{"a match on the tail of a long field", matching(`TAILMARK`), withField(f, long), chat.ReplayUnmatched},
				{"a match on the end of the head", matching(`TAIL$`), withField(f, long), chat.ReplayMatched},
				{"a match on the tail of a field at the cap", matching(`TAIL$`), withField(f, head), chat.ReplayMatched},
				{"a match that needs the byte after the cap", matching(`TAILM`), withField(f, long), chat.ReplayUnmatched},
				{"an exclude on the head of a long field", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Exclude = map[v1alpha1.MessageField]string{f: `HEADMARK`} }),
					withField(f, long), chat.ReplayExcluded},
				{"an exclude on the tail of a long field", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Exclude = map[v1alpha1.MessageField]string{f: `TAILMARK`} }),
					withField(f, long), chat.ReplayMatched},
				{"an extract from the head of a long field", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
					tr.Extract = map[string]v1alpha1.Extract{"mark": {Field: f, Regex: `(?P<mark>HEADMARK)`}}
				}), withField(f, long), chat.ReplayMatched},
				{"an extract from the tail of a long field", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
					tr.Extract = map[string]v1alpha1.Extract{"mark": {Field: f, Regex: `(?P<mark>TAILMARK)`}}
				}), withField(f, long), chat.ReplayUnmatched},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := classify(t, tc.tr, tc.msg); got.Outcome != tc.want {
						t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Why, tc.want)
					}
				})
			}
		})
	}
}

// The cut is on a UTF-8 boundary, and an extract is the text before it: a value is never cut in a character, and it
// is at most the cap.
func TestAFieldIsCutOnACharacterBoundary(t *testing.T) {
	extractAll := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Extract = map[string]v1alpha1.Extract{"all": {Field: v1alpha1.FieldBody, Regex: `(?s)(?P<all>.*)`}}
	})
	for _, tc := range []struct {
		name string
		body string
		want int // the bytes of the value
	}{
		{"ASCII past the cap", strings.Repeat("a", fieldCap+500), fieldCap},
		{"under the cap", strings.Repeat("a", fieldCap-1), fieldCap - 1},
		{"a character that ends at the cap", strings.Repeat("a", fieldCap-2) + "é" + "TAIL", fieldCap},
		{"a character that starts before the cap and ends after it", strings.Repeat("a", fieldCap-1) + "é" + "TAIL", fieldCap - 1},
		{"a three-byte character split after its first byte", strings.Repeat("a", fieldCap-1) + "€" + "TAIL", fieldCap - 1},
		{"a three-byte character split after its second byte", strings.Repeat("a", fieldCap-2) + "€" + "TAIL", fieldCap - 2},
		{"a four-byte character split after its third byte", strings.Repeat("a", fieldCap-3) + "😀" + "TAIL", fieldCap - 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(t, extractAll, withField(v1alpha1.FieldBody, tc.body))
			if got.Outcome != chat.ReplayMatched {
				t.Fatalf("outcome = %q (%s)", got.Outcome, got.Why)
			}
			v := got.Fields["all"]
			if len(v) != tc.want {
				t.Errorf("the value is %d bytes, want %d", len(v), tc.want)
			}
			if !strings.HasPrefix(tc.body, v) {
				t.Errorf("the value is not the start of the field")
			}
			if strings.ContainsRune(v, '�') {
				t.Errorf("the value was cut inside a character")
			}
		})
	}
}

// The live intake matches by the same rule: a message whose mark is past the cap starts nothing, and the same message
// with the mark in its head starts a turn.
func TestTheIntakeMatchesOnlyTheHeadOfAField(t *testing.T) {
	filler := strings.Repeat("x", fieldCap)
	for _, tc := range []struct {
		name  string
		body  string
		turns int
	}{
		{"the mark in the head", "FIRING " + filler, 1},
		{"the mark past the cap", filler + " FIRING", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTriggered(t, botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Match[v1alpha1.FieldBody] = `FIRING` }))
			if err := h.in.Message(h.ctx, withField(v1alpha1.FieldBody, tc.body)); err != nil {
				t.Fatal(err)
			}
			if got := len(h.got()); got != tc.turns {
				t.Fatalf("turns started = %d, want %d", got, tc.turns)
			}
		})
	}
}
