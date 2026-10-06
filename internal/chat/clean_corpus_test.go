package chat_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// Clean deletes the one set that redaction does, for display: every class of the corpus, from the middle of a word.
func TestCleanDeletesEveryInvisible(t *testing.T) {
	for _, inv := range redacttest.Invisibles {
		for _, line := range []bool{false, true} {
			if got := chat.Clean("pass"+inv.Text+"word", line); got != "password" {
				t.Errorf("%s: Clean(line=%t) = %q, want the character gone", inv.Name, line, got)
			}
		}
	}
}

// Every kind of line break is "\n", and a line is one line whatever the break.
func TestCleanWritesEveryLineBreakAsNewline(t *testing.T) {
	for _, b := range redacttest.LineBreaks {
		if got := chat.Clean("a"+b.Text+"b", false); got != "a\nb" {
			t.Errorf("%s: Clean = %q, want a\\nb", b.Name, got)
		}
		if got := chat.Clean(" a"+b.Text+b.Text+"b ", true); got != "a b" {
			t.Errorf("%s: Clean(line) = %q, want a b", b.Name, got)
		}
	}
}

// What Clean shows and what redaction reads are one text: for any text of the corpus, Clean without the line flag is
// Normalize, and with it Normalize on one line. Clean adds the line flag and nothing else.
func TestCleanIsTheNormalizerAndItsLineFlag(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		n := redact.Normalize(c.Text)
		if got := chat.Clean(c.Text, false); got != n {
			t.Errorf("%s: Clean = %q, Normalize = %q", c.Name, got, n)
		}
		got := chat.Clean(c.Text, true)
		if want := strings.Join(strings.Fields(n), " "); got != want {
			t.Errorf("%s: Clean(line) = %q, want %q", c.Name, got, want)
		}
		redacttest.CheckNormalized(t, got) // Clean redacts nothing: it is the invisible characters that must be gone
	}
}
