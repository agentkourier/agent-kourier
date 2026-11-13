package retention

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of the retention loop. The leader builds the loop
// without a reply age or a session grace, so the loop's defaults are the values. Each interface is given as a struct that
// embeds it and holds none: New only checks that one is set, and nothing is called.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "auditRetentionMin", MinAuditRetention)

	r, err := New(Config{
		Store:      struct{ Store }{},
		ThreadTTLs: func() (map[string]time.Duration, bool) { return nil, false },
		Canceller:  struct{ TaskCanceller }{}, Audit: struct{ StaleAuditor }{},
		Holds:     func(store.ConversationKey, string) bool { return false },
		OnRemoved: func(string, int) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	limitsdoc.Duration(t, "sessionGrace", r.cfg.SessionGrace)
	limitsdoc.Duration(t, "strandedReply", r.cfg.ReplyAge)
}
