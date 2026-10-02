package chat_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

func TestMessageIDIsTheStringSlackAlreadyStores(t *testing.T) {
	got := chat.MessageID(chat.Ref{Platform: "slack", Conn: "courier-system/slack-main", Channel: "C0123456789", ID: "1699999999.000100"})
	if want := "slack:courier-system/slack-main:C0123456789:1699999999.000100"; got != want {
		t.Errorf("MessageID = %q, want %q: the A2A messageId of a message must not change", got, want)
	}
	if chat.MessageID(chat.Ref{Platform: "other", Conn: "a/b", Channel: "c", ID: "d"}) == chat.MessageID(chat.Ref{Platform: "slack", Conn: "a/b", Channel: "c", ID: "d"}) {
		t.Error("two platforms' messages got the same ID")
	}
}

func TestClean(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		line     bool
		want     string
	}{
		{"plain text is left alone", "Hello, 日本語 🎉", false, "Hello, 日本語 🎉"},
		{"zero-width and bidi characters go", "a\u200bb\u200dc\u202ed\u2066e\ufeff\u2060f\u00ad", false, "abcdef"},
		{"they cannot split a broadcast apart", "<\u200b!chan\u200bnel>", false, "<!channel>"},
		{"control characters go, tab and newline stay", "a\x00b\x07c\x1b[31m\x7fd\te\nf", false, "abc[31md\te\nf"},
		{"every line break is a newline", "a\r\nb\rc\u0085d\u2028e\u2029f", false, "a\nb\nc\nd\ne\nf"},
		{"invisible tag characters go", "x\U000E0041\U000E007Fy", false, "xy"},
		{"blank fillers go: Hangul filler and braille blank", "a\u3164\u2800b", false, "ab"},
		{"private use and invalid UTF-8 go", "ab\xffc", false, "abc"},
		{"a line is one line", "  a\n\n b\t c \r\n", true, "a b c"},
		{"nothing left", "\u200b\u200c\n", true, ""},
	} {
		if got := chat.Clean(tc.in, tc.line); got != tc.want {
			t.Errorf("%s: Clean(%q, %t) = %q, want %q", tc.name, tc.in, tc.line, got, tc.want)
		}
	}
}

func TestTruncateCutsAtTheLimitInUTF16UnitsAndMarksIt(t *testing.T) {
	if got := chat.Truncate("short", 100); got != "short" {
		t.Errorf("a text under the limit changed: %q", got)
	}
	long := strings.Repeat("a", 500)
	got := chat.Truncate(long, 100)
	if chat.Units(got) > 100 || !strings.HasSuffix(got, chat.Truncated) || !strings.HasPrefix(got, "aaa") {
		t.Errorf("Truncate = %q (%d units), want a prefix of a's ending in the marker, within 100", got, chat.Units(got))
	}
	// A rune outside the BMP is two units, and is never split.
	emoji := strings.Repeat("🎉", 100)
	got = chat.Truncate(emoji, 50)
	if chat.Units(got) > 50 || strings.ContainsRune(got, '�') {
		t.Errorf("Truncate of astral runes = %q (%d units), want at most 50 and no split rune", got, chat.Units(got))
	}
	if got := chat.Truncate(long, 3); got != string([]rune(chat.Truncated)[:3]) {
		t.Errorf("a limit under the marker's length gave %q, want the start of the marker", got)
	}
	if got := chat.Truncate(long, 0); got != "" {
		t.Errorf("a zero limit gave %q", got)
	}
}

func TestCapsLimitIsPerPlaceWithTheDefaultBehindIt(t *testing.T) {
	c := chat.Caps{MaxText: 1000, Limits: map[chat.Where]int{chat.WhereHeader: 150}}
	if c.Limit(chat.WhereHeader) != 150 || c.Limit(chat.WhereBody) != 1000 {
		t.Errorf("Limit(header) = %d, Limit(body) = %d, want 150 and 1000", c.Limit(chat.WhereHeader), c.Limit(chat.WhereBody))
	}
}

func TestWhereClassesAreExactlyTheRichAndLinePlaces(t *testing.T) {
	var rich, line []string
	for _, w := range chat.AllWheres() {
		if w.Rich() && w.Line() {
			t.Errorf("%s is both rich and a line", w)
		}
		if w.Rich() {
			rich = append(rich, w.String())
		}
		if w.Line() {
			line = append(line, w.String())
		}
	}
	if !reflect.DeepEqual(rich, []string{"body", "stream"}) {
		t.Errorf("rich places = %v, want body and stream only", rich)
	}
	if len(line) == 0 || len(chat.AllWheres()) != 15 {
		t.Errorf("%d places, %d lines; a place was added without deciding its class", len(chat.AllWheres()), len(line))
	}
}

var everyPart = chat.Message{Fallback: "fallback", Parts: []chat.Part{
	chat.Header{Text: "head"}, chat.Body{Markdown: "*body*"}, chat.Context{Text: "ctx"}, chat.Note{Text: "note"},
	chat.ToolLine{Name: "kubectl", Summary: "ns=\"prod\""},
	chat.Buttons{Buttons: []chat.Button{{ID: "go", Label: "Go", Value: "ix-1", Primary: true}}},
}}

func TestOutgoingSurvivesTheOutboxRoundTrip(t *testing.T) {
	deadline := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	prompt := chat.Prompt{ID: "ix-1", Hint: "Which?", Questions: []chat.Question{{Text: "Env?", Choices: []string{"prod", "dev"}, Multiple: true}, {Text: "Why?"}}, Deadline: deadline, Subagent: "sre"}
	for name, o := range map[string]chat.Outgoing{
		"a message with every part": chat.Say(everyPart),
		"a prompt":                  chat.Ask(prompt),
		"an ending":                 chat.End(chat.Ending{Outcome: chat.Answered, By: "U1", At: deadline, Prompt: prompt, Answers: [][]string{{"0", "1"}, {"because"}}}),
	} {
		raw, err := json.Marshal(o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var back chat.Outgoing
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(o, back) {
			t.Errorf("%s: stored and read back as\n%+v\nwant\n%+v", name, back, o)
		}
		if err := back.Validate(); err != nil {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}

func TestAPartOfAnUnknownKindIsRefusedNotGuessedAt(t *testing.T) {
	var o chat.Outgoing
	err := json.Unmarshal([]byte(`{"v":1,"message":{"parts":[{"kind":"carousel"}]}}`), &o)
	if !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Unmarshal = %v, want ErrInvalid: a payload from a newer version is not guessed at", err)
	}
}

func TestValidateRefusesWhatCannotBeDelivered(t *testing.T) {
	prompt := chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "q"}}}
	for name, o := range map[string]chat.Outgoing{
		"a payload of another version":   {V: 2, Message: &chat.Message{Fallback: "x"}},
		"a legacy payload with no V":     {Message: &chat.Message{Fallback: "x"}},
		"nothing set":                    {V: chat.OutgoingVersion},
		"two forms set":                  {V: chat.OutgoingVersion, Message: &chat.Message{Fallback: "x"}, Prompt: &prompt},
		"an empty message":               chat.Say(chat.Message{}),
		"a button with no label":         chat.Say(chat.Message{Parts: []chat.Part{chat.Buttons{Buttons: []chat.Button{{ID: "a"}}}}}),
		"a prompt with no ID":            chat.Ask(chat.Prompt{Questions: prompt.Questions}),
		"a prompt with no question":      chat.Ask(chat.Prompt{ID: "ix-1"}),
		"an ending with an unknown kind": chat.End(chat.Ending{Outcome: "vanished"}),
		"a nil part":                     chat.Say(chat.Message{Fallback: "x", Parts: []chat.Part{chat.Header{Text: "h"}, nil}}),
		"a part by pointer":              chat.Say(chat.Message{Fallback: "x", Parts: []chat.Part{&chat.Header{Text: "h"}}}),
	} {
		if err := o.Validate(); !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
	if err := chat.Say(everyPart).Validate(); err != nil {
		t.Errorf("a whole message: Validate = %v", err)
	}
}

func TestAMessageWithAPartOfAForeignTypeCannotBeStored(t *testing.T) {
	_, err := json.Marshal(chat.Message{Parts: []chat.Part{nil}})
	if !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Marshal = %v, want ErrInvalid", err)
	}
}
