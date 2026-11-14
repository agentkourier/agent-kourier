package feedback

import (
	"context"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// noEphemeral is an interact.Ephemeral that sends nothing: the interface cannot be embedded, since its one method has its name.
type noEphemeral struct{}

func (noEphemeral) Ephemeral(context.Context, chat.Target, string, chat.Message) error { return nil }

// TestLimitsPageOfThePressLimits holds the page docs/reference/limits.md to the press limits of the feedback buttons, which the
// broker leaves to the Handler's defaults, as it does for the buttons of a prompt. Each interface is given as a struct that embeds
// it and holds none: New only checks that one is set, and nothing is called.
func TestLimitsPageOfThePressLimits(t *testing.T) {
	h, err := New(Config{
		Store: struct{ Store }{}, Sessions: struct{ interact.SessionReader }{}, Authorizers: chattest.Same[interact.Authorizer](struct{ interact.Authorizer }{}),
		Ephemerals: chattest.Same[interact.Ephemeral](noEphemeral{}), Auditor: struct{ Auditor }{}, Notify: func(store.Press) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	limitsdoc.Count(t, "pressTotal", h.cfg.Limits.Total)
	limitsdoc.Count(t, "pressPerUser", h.cfg.Limits.PerUser)
}
