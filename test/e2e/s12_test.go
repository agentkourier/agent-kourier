//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestS12_ATurnThatEndsWithoutOutputIsTold: the agent's task fails (scenario-fails, with a provider's
// masked-key error as its status message) or completes with nothing to show, and the thread gets courier's own
// notice, once, and none of the agent's text. A turn must never end in silence: this is the dogfood failure, where
// a model call that failed upstream left the thread empty. The outcome counter goes up by one for each.
//
// The fast tier only: the full tier's kagent has no agent of that name, so its Binding for CS12 points nowhere.
func TestS12_ATurnThatEndsWithoutOutputIsTold(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skip("scenario-fails is a stub agent of the fast tier; the full tier's kagent does not have it")
	}
	for _, tc := range []struct {
		name, prompt, outcome, notice string
	}{
		{"a failed task", "do something", "failed", "The agent's task failed"},
		{"a completed task with no output", "say something empty", "completed", "The agent finished without any output."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{"binding": "courier/fast-s12", "outcome": tc.outcome}
			const metric = "courier_turn_outcomes_total"
			before := s.counter(metric, labels)

			m := s.mention(ChannelS12, "US12", tc.prompt)
			th := s.thread(ChannelS12, m.TS, "courier's notice", func(th []fakeslack.MessageView) bool {
				for _, b := range BotMessages(th) {
					if strings.Contains(b.Text, tc.notice) {
						return true
					}
				}
				return false
			})
			// The status message of the failed task has a key's last characters in it: the thread shows no part of it.
			for _, b := range BotMessages(th) {
				if strings.Contains(b.Text, "wxyz") || strings.Contains(b.Text, "401") {
					t.Errorf("the thread says %q: the agent's status message reached it", b.Text)
				}
			}
			// One notice, and nothing else said: the turn has no output to show.
			err := Consistently(s.ctx, 3*time.Second, "the thread holds the one notice", func(ctx context.Context) (bool, error) {
				th, err := s.slack.Thread(ctx, ChannelS12, m.TS)
				return len(BotMessages(th)) == 1, err
			})
			if err != nil {
				t.Error(err)
			}
			err = Wait(s.ctx, 30*time.Second, "the turn-outcome counter to go up", func(ctx context.Context) (bool, error) {
				v, err := s.courier.Counter(ctx, metric, labels)
				return v > before, err
			})
			if err != nil {
				t.Fatalf("%v (it was %v)", err, before)
			}
			if got := s.counter(metric, labels); got != before+1 {
				t.Errorf("%s%v went from %v to %v; want one more: the turn is counted once", metric, labels, before, got)
			}
		})
	}
}
