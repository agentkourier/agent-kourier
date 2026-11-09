package outbox

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestLimitsPage holds the page docs/reference/limits.md to how the outbox retries and expires a post. The broker sets none of
// these in Config, so the defaults that New fills in are the values. Store and Platforms are given as structs that embed the
// interface and hold none: New only checks that they are set, and nothing is called.
func TestLimitsPage(t *testing.T) {
	o, err := New(Config{Store: struct{ store.Outbox }{}, Platforms: chattest.Same[Platform](struct{ Platform }{}), OnAbandon: func(string, string) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	limitsdoc.Duration(t, "outboxRetention", o.cfg.Retention)
	limitsdoc.Duration(t, "outboxRetryBase", o.cfg.RetryBase)
	limitsdoc.Duration(t, "outboxRetryMax", o.cfg.RetryMax)
	limitsdoc.Duration(t, "outboxCall", o.cfg.CallTimeout)
	limitsdoc.Duration(t, "outboxInterval", o.cfg.Interval)
}
