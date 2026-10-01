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
//   - scenario-memory says how many turns its context has had and quotes the earlier ones:
//     `Message 3. Earlier: "one", "two".`, or `Message 1. Earlier: none.` for a new context.
//   - scenario-ask asks the two questions of spike-ask's "two questions" (a multi-select and a
//     free-text one), then echoes exactly what it received, one entry per answered question:
//     `Answers: "PostgreSQL, MySQL" | "blue".`, an empty entry shown as (none).
//
// The scenarios and the agents that serve them in the cluster:
//
//	S1   see WP-14g       spike-slow's eight ticks take 2.1 s at the default tick of
//	                      cmd/stubagent (300 ms), about the 2 s edit throttle: the final text and
//	                      at most one update, so live output is not shown
//	S2   scenario-memory  "Message 3" with the earlier texts after a courier restart proves the
//	                      contextId was kept; "Message 1" with none proves it was lost
//	S3   scenario-ask     "Answers: ... | (none)." means an incomplete answer reached the agent.
//	                      Neither the stub nor spike-ask rejects one (spike-ask says "Both
//	                      answered." to [[x], []]), so courier's check is the only guard
//	S4   see WP-14g       scenario-slow runs about six seconds, a window and not a gate
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
	s.Agent(ns, "scenario-ask").Reply(func(string) []Step {
		return []Step{Ask([]Question{
			{Question: "Which databases?", Choices: []string{"PostgreSQL", "MySQL"}, Multiple: true},
			{Question: "Favorite color?"},
		}, func(r Response) []Step {
			if r.Plain() {
				return plain()
			}
			entries := make([]string, len(r.Answers))
			for i, a := range r.Answers {
				entries[i] = "(none)"
				if len(a) > 0 {
					entries[i] = strconv.Quote(strings.Join(a, ", "))
				}
			}
			return []Step{Text("Answers: " + strings.Join(entries, " | ") + ".")}
		})}
	})
}
