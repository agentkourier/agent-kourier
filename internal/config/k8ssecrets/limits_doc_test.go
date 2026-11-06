package k8ssecrets

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how a Secret that the API server denied is tried again. The broker
// leaves the retry interval to its default unless the reloader's recheck is set, which production does not.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "deniedRetry", New(nil).retry)
	limitsdoc.Count(t, "deniedRetryBurst", deniedRetryBurst)
	limitsdoc.Duration(t, "deniedRetryPace", New(nil).pace)
	limitsdoc.Count(t, "deniedRetryRate", int(time.Second/deniedRetryPace))
}
