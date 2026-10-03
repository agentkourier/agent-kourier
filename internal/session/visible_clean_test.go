package session_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// r is the string of the code points cps, so that this file holds no invisible character.
func r(cps ...rune) string { return string(cps) }

// raw is the string of the bytes bs, which may be invalid UTF-8.
func raw(bs ...byte) string { return string(bs) }

// session cannot import chat (chat imports session), so session.visible repeats the rule of chat.Clean that
// decides whether text shows: the renderer drops an artifact that is blank once cleaned (render.Body). This
// pins the two together: if Clean learns to delete another kind of character, or visible another, this fails.
func TestVisibleAgreesWithWhatTheRendererShows(t *testing.T) {
	for _, s := range []string{
		"", " ", "\n\t \r\n", "text", " text ", "a",
		r(0x200b), r(0x200b, 0x200c, 0x200d, 0x2060, 0xfeff), // zero-width: Cf
		r(0x00ad), r(0x202e), r(0x2066), // soft hyphen and bidi controls: Cf
		r(0x3164), r(0x2800), r(0x3164, 0x2800, ' ', 0x200b), // the blank fillers
		r(0), r(1, 0x1b, 0x7f), r(0x85), r(0x2028, 0x2029), // controls and line separators
		r(0xe0041), r(0xe000), r(0xf0000), // tag characters and private use
		raw(0xff), raw(0xff, 0xfe), raw(0xc3), raw(0xed, 0xa0, 0x80), // invalid UTF-8 and an encoded surrogate
		r(0xfffd), r(0xfffd, 0x200b), // a real replacement character is text
		r(0xa0), r(0xa0, 0x200b), r(0x2003), r(0x1680), // other white space
		r(' ', 0x200b, 0x2800) + "x" + r(0x3164, ' '), raw(0xff) + "x", r(0x200b) + ".", r(0x3164, 0x3164) + "a",
		r(0x1f600), r(0x301), r(0x200d, 0x1f600),
	} {
		if got, want := session.Visible(s), strings.TrimSpace(chat.Clean(s, false)) != ""; got != want {
			t.Errorf("visible(%q) = %v; chat.Clean says %v", s, got, want)
		}
	}
}
