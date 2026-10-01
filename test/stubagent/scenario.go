package stubagent

import (
	"fmt"
	"strconv"
	"strings"
)

// ScenarioSlowTicks is how many ticks scenario-slow streams. At cmd/stubagent's default tick it
// runs about six seconds, a window in which a second message reliably meets a running task.
const ScenarioSlowTicks = 20

// ScenarioAgents registers the agents that only the Phase 1 E2E scenarios need, in the namespace
// kagent, beside SpikeAgents:
//
//   - scenario-slow streams ScenarioSlowTicks ticks, "tick 1 " to "tick 20 ", paced by WithTick.
//   - scenario-memory says how many messages its context has had and quotes the earlier ones:
//     `Message 3. Earlier: "one", "two".`, or `Message 1. Earlier: none.` for a new context.
//
// The scenarios and the agents that serve them in the cluster (the stub runs with the default
// tick of cmd/stubagent, 300 ms, which spike-slow's eight ticks fill in 2.1 s):
//
//	S1   spike-slow       eight streamed chunks into a new thread
//	S2   scenario-memory  "Message 3" with the earlier texts after a courier restart proves the
//	                      contextId was kept; "Message 1" with none proves it was lost
//	S3   spike-ask        send "two questions" in the prompt: a multi-choice question and a free
//	                      text one. "Both answered." follows only a complete answer; the stub
//	                      itself accepts a partial one, so courier's check is the only guard
//	S4   scenario-slow    about six seconds, where spike-slow's 2.1 s is too short to queue
//	                      a reply reliably
//	S5   test-agent       one instant chunk, so the kill lands between post and record
//	S11  spike-approve    "I have added 3 and 5..." would mean the tool ran; courier's denial
//	                      gives "Resumed after the tool result."
func ScenarioAgents(s *Server) {
	const ns = "kagent"
	s.Agent(ns, "scenario-slow").Reply(func(string) []Step {
		var ticks []string
		for n := 1; n <= ScenarioSlowTicks; n++ {
			ticks = append(ticks, "tick "+strconv.Itoa(n)+" ")
		}
		return []Step{Text(ticks...)}
	})
	s.Agent(ns, "scenario-memory").ReplyWithHistory(func(prior []string, _ string) []Step {
		earlier := "none"
		if len(prior) > 0 {
			quoted := make([]string, len(prior))
			for i, p := range prior {
				quoted[i] = strconv.Quote(p)
			}
			earlier = strings.Join(quoted, ", ")
		}
		return []Step{Text(fmt.Sprintf("Message %d. Earlier: %s.", len(prior)+1, earlier))}
	})
}
