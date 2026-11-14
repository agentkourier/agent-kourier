//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestS13_AToolCallIsAStepCardAndAFailedOneIsAnError: the agent says something, calls a tool that works, says
// something, calls a tool that fails, and says the rest (scenario-tools). The reply is one streamed message, which
// ends with each piece of the text once, in order, and a task card for each call, in the order of the calls: the
// first done, the second an error, and none left in progress. The cards are what a person sees of the tool calls.
//
// Both tiers: the full tier's scenario model has kagent call the tools of the scenario MCP server.
//
// UNCONFIRMED(full tier): that kagent's function_response for an MCP tool that reports isError has the "error" entry
// that Agent Kourier reads as a failed call (render.Failed), and that it streams the text said before a call as part of the
// reply. The assertions do not depend on how the text is split into artifacts, only on each piece being said once.
func TestS13_AToolCallIsAStepCardAndAFailedOneIsAnError(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier == "ax" {
		t.Skip("the AX tier's agent has none of the scenario's tools (check_status, restart_service)")
	}
	m := s.mention(ChannelS13, "US13", "check the api service and restart it")

	pieces := []string{"Checking the service.", "It is up, so I will restart it.", "The restart was refused."}
	th := s.thread(ChannelS13, m.TS, "the end of the reply", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && finished(bots[0]) && strings.Contains(bots[0].Text, pieces[len(pieces)-1])
	})
	reply := BotMessages(th)[0]

	// The text, once each and in order: neither the stream's end nor a step adds a second copy (found in dogfood for
	// S1, 2026-10-03: the text was shown twice).
	prev := -1
	for _, p := range pieces {
		at := strings.Index(reply.Text, p)
		if at <= prev || strings.Count(reply.Text, p) != 1 {
			t.Errorf("the reply says %q: want %q once, after what came before it", reply.Text, p)
		}
		if n := strings.Count(string(reply.Blocks), p); n != 1 {
			t.Errorf("the reply's blocks %s hold %q %d times; want once", reply.Blocks, p, n)
		}
		prev = at
	}

	// The cards, in the order of the calls, by what each call came to; none is still running.
	cards := Tasks(reply)
	type card struct{ title, status string }
	var got []card
	for _, c := range cards {
		got = append(got, card{c.Title, c.Status})
	}
	want := []card{{"check_status", "complete"}, {"restart_service", "error"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("task cards %+v; want %+v: done for the call that worked, error for the one that failed", cards, want)
	}
	if cards[0].ID == cards[1].ID {
		t.Errorf("both cards are %q; want one step for each call", cards[0].ID)
	}
	if d := cards[0].Details; d != `service="api"` {
		t.Errorf("the first card's details are %q; want the call's arguments, service=\"api\"", d)
	}
	// A call whose result came is done. One that was left without its result is closed at the stream's end as complete
	// with a note that no result was reported, which the status alone does not tell from done.
	if cards[0].Output != "" {
		t.Errorf("the first card says %q; want none: its call returned, so it is done and not closed at the stream's end", cards[0].Output)
	}
	for _, c := range cards {
		if c.Status == "in_progress" || c.Status == "pending" {
			t.Errorf("card %+v is not finished though the reply is", c)
		}
	}

	// One message, and nothing else said in the thread.
	err := Consistently(s.ctx, 3*time.Second, "the thread holds the one message of the bot", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS13, m.TS)
		return len(BotMessages(th)) == 1, err
	})
	if err != nil {
		t.Error(err)
	}
}
