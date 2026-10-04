//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// AX exercises the same authorization and durable decision path with a thread reply.
func axTextQuestion(t *testing.T, s *scenario) {
	const user = "UAXANSWER"
	s.know(user)
	if err := s.slack.SetMembers(s.ctx, ChannelS3, user); err != nil {
		t.Fatal(err)
	}
	m := s.mention(ChannelS3, user, "ask "+s.stub.Gate("AX-question"))
	s.thread(ChannelS3, m.TS, "the plain-text question", func(th []fakeslack.MessageView) bool {
		for _, b := range BotMessages(th) {
			if HasButton(b.Blocks, submitAction) && strings.Contains(string(b.Blocks), "Which service?") && strings.Contains(string(b.Blocks), "Reply in this thread") {
				return true
			}
		}
		return false
	})
	// An outsider cannot resolve the question or cause an agent continuation.
	s.reply(ChannelS3, m.TS, "UAXOUTSIDER", "unauthorized-answer")
	if err := Consistently(s.ctx, 2*time.Second, "an unauthorized reply cannot answer the question", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS3, m.TS)
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "Answers:") {
				return false, err
			}
		}
		return true, err
	}); err != nil {
		t.Fatal(err)
	}
	restart, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
	defer cancel()
	if err := s.pods.RolloutRestart(restart, kube.CourierNamespace, kube.CourierDeployment); err != nil {
		t.Fatal(err)
	}
	s.connected()
	answer := "service-" + s.stub.Gate("AX-answer")
	s.reply(ChannelS3, m.TS, user, answer)
	s.thread(ChannelS3, m.TS, "the persisted question to take the authorized reply", func(th []fakeslack.MessageView) bool {
		for _, b := range BotMessages(th) {
			if finished(b) && strings.Contains(b.Text, "Answers:") && strings.Contains(b.Text, answer) {
				return true
			}
		}
		return false
	})
	if err := Consistently(s.ctx, 3*time.Second, "exactly one continuation after the answer", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS3, m.TS)
		n := 0
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "Answers:") {
				n++
			}
		}
		return n == 1, err
	}); err != nil {
		t.Fatal(err)
	}
}

// Exercise AX suspend/resume, then Substrate DATA pause with AX cold-boot resume.
// S2 checks that the next real Slack turn still quotes both earlier ADK turns.
func axSuspendResume(t *testing.T, s *scenario, actor string) {
	t.Helper()
	c := newClient(s.env.StubURL)
	c.hc.Timeout = 2 * time.Minute
	for _, action := range []string{"suspend", "resume", "pause", "resume"} {
		if err := c.do(s.ctx, http.MethodPost, "/_control/task/"+actor+"/"+action, nil, nil); err != nil {
			t.Fatal(err)
		}
		phase := "Suspended"
		if action != "suspend" {
			phase = "Running"
		}
		if err := Wait(s.ctx, 2*time.Minute, "AX task "+actor+" to be "+phase, func(ctx context.Context) (bool, error) {
			var task struct {
				ActorState string `json:"actorState"`
				Status     struct {
					Phase string `json:"phase"`
				}
			}
			err := c.do(ctx, http.MethodGet, "/_control/task/"+actor+"/status", nil, &task)
			state := "ACTOR_STATE_SUSPENDED"
			switch action {
			case "resume":
				state = "ACTOR_STATE_RUNNING"
			case "pause":
				state = "ACTOR_STATE_PAUSED"
			}
			return task.Status.Phase == phase && task.ActorState == state, err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAX_AQuestionDeadlineCancelsTheTask(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier != "ax" {
		t.Skip("AX-only short-deadline Binding")
	}
	const channel = "CAXTIMEOUT"
	m := s.mention(channel, "UAXTIMEOUT", "ask "+s.stub.Gate("AX-timeout"))
	s.thread(channel, m.TS, "the question to be posted before timing expiry", func(th []fakeslack.MessageView) bool {
		for _, b := range BotMessages(th) {
			if HasButton(b.Blocks, submitAction) || strings.Contains(strings.ToLower(b.Text), "timed out") {
				return true
			}
		}
		return false
	})
	s.thread(channel, m.TS, "the question to expire", func(th []fakeslack.MessageView) bool {
		for _, b := range BotMessages(th) {
			if strings.Contains(strings.ToLower(b.Text), "timed out") {
				return true
			}
		}
		return false
	})
	if err := Consistently(s.ctx, 3*time.Second, "expiry must not invent an answer", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, channel, m.TS)
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "Answers:") {
				return false, err
			}
		}
		return true, err
	}); err != nil {
		t.Fatal(err)
	}
}
