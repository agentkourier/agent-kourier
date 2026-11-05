package session_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// Every fixed sentence of the thread that Agent Kourier writes for a turn that stopped, or a message it did not send, is shown whole. A
// notice's detail is cut at render.MaxNoticeDetail units, and a sentence longer than that lost its last words, which for the lookup was
// what a person is to do. The test is of the notice as the thread gets it, through render.Notice, with the words of a failed request
// that the live sink puts before the detail.
func TestFixedNoticeDetailsAreShownWhole(t *testing.T) {
	for name, detail := range session.FixedDetails {
		t.Run(name, func(t *testing.T) {
			if n := chat.Units(detail); n > render.MaxNoticeDetail {
				t.Errorf("the detail is %d units, and the thread shows %d: %q", n, render.MaxNoticeDetail, detail)
			}
			if got := render.Notice("This request failed, and I won't try it again.", detail).Fallback; !strings.HasSuffix(got, detail) || strings.HasSuffix(got, chat.Truncated) {
				t.Errorf("the thread shows %q, which does not end with the whole detail", got)
			}
		})
	}
}
