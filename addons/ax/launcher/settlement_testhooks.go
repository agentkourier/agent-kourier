//go:build testhooks

package launcher

import (
	"context"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The kind fixture keeps this turn open after receipt until its leader stops.
func beforeSettlement(ctx context.Context, task *a2a.Task) bool {
	if len(task.History) != 0 {
		text, _ := messageText(task.History[0])
		if strings.Contains(text, "AX_RESULT_STALL_SETTLEMENT") {
			<-ctx.Done()
			return false
		}
	}
	return true
}
