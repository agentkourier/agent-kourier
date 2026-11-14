package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// tooLarge is the answer that is too large, and permanent is anything that a retry would get again: the agent card past its bound is
// permanent (a reconnect that meets it meets it again) and no answer past a bound.
func TestTooLargeIsNotTheCardAndPermanentIsBoth(t *testing.T) {
	answer := fmt.Errorf("get task t1: %w: more than %d bytes", dialect.ErrResponseTooLarge, dialect.MaxResponseBytes)
	card := fmt.Errorf("connect to agent: %w: %w", dialect.ErrCardTooLarge, dialect.ErrResponseTooLarge)
	for _, tc := range []struct {
		name                string
		err                 error
		wantLarge, wantPerm bool
	}{
		{"an answer past its bound", answer, true, true},
		{"an agent card past its bound", card, false, true},
		{"a failed connection", errors.New("connection refused"), false, false},
	} {
		if got := tooLarge(tc.err); got != tc.wantLarge {
			t.Errorf("%s: tooLarge = %v, want %v", tc.name, got, tc.wantLarge)
		}
		if got := permanent(tc.err); got != tc.wantPerm {
			t.Errorf("%s: permanent = %v, want %v", tc.name, got, tc.wantPerm)
		}
	}
}
