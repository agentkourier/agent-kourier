package dialect_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to what the broker reads of a backend's responses.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Bytes(t, "cardBytes", dialect.MaxCardBytes)
	limitsdoc.Bytes(t, "eventBytes", dialect.MaxEventBytes)
	limitsdoc.Bytes(t, "answerBytes", dialect.MaxResponseBytes)
}
