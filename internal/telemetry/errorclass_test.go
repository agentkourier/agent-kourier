package telemetry

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// A response past a bound has a class of its own, which a metric label or a span can carry and an operator can alert on:
// it is neither an agent's error nor a failed connection, and it names no size and nothing the backend sent.
func TestErrorClassOfAResponseThatIsTooLarge(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an answer past the bound", fmt.Errorf("get task t1: %w: more than 4194304 bytes", dialect.ErrResponseTooLarge), "response_too_large"},
		{"an event past the bound, wrapped twice", fmt.Errorf("send: %w", fmt.Errorf("%w: an event of more than 2097152 bytes", dialect.ErrResponseTooLarge)), "response_too_large"},
		{"a cancelled call is still cancelled", fmt.Errorf("%w: %w", dialect.ErrResponseTooLarge, context.Canceled), "cancelled"},
		{"any other error", errors.New("boom"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorClass(tc.err); got != tc.want {
				t.Errorf("ErrorClass = %q, want %q", got, tc.want)
			}
		})
	}
}
