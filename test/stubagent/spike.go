package stubagent

import (
	"slices"
	"strconv"
	"strings"
)

// SpikeAgents registers the five agents the WP-02 spike (docs/spikes/wp02-a2a.md) drove, in the namespace
// kagent, scripted as the spike's mock models answered. Running the spike against a stub that
// has them is the acceptance check for this package; the exchanges they reproduce are in
// test/fixtures/kagent/. The prompts that pick a script are the spike's.
//
//   - test-agent says "The answer is 4." to anything.
//   - spike-slow streams eight ticks, "tick 1 " to "tick 8 ", paced by WithTick.
//   - spike-ask asks which database to use, or, when the prompt says "two questions", asks two.
//   - spike-nested asks which region, after a transfer to a sub-agent; the question is flat.
//   - spike-approve asks approval to run add_numbers(3, 5).
func SpikeAgents(s *Server) {
	const ns = "kagent"
	dbs := []string{"PostgreSQL", "MySQL"}
	s.Agent(ns, "test-agent").Reply(func(string) []Step { return []Step{Text("The answer is 4.")} })
	s.Agent(ns, "spike-slow").Reply(func(string) []Step {
		var ticks []string
		for n := 1; n <= 8; n++ {
			ticks = append(ticks, "tick "+strconv.Itoa(n)+" ")
		}
		return []Step{Text(ticks...)}
	})
	s.Agent(ns, "spike-ask").Reply(func(prompt string) []Step {
		if strings.Contains(prompt, "two questions") {
			return []Step{Ask([]Question{
				{Question: "Which databases?", Choices: dbs, Multiple: true},
				{Question: "Favorite color?"},
			}, func(r Response) []Step {
				switch {
				case r.Plain():
					return plain()
				case len(r.Answers) == 2:
					return []Step{Text("Both answered.")}
				}
				return []Step{Text("Using " + firstAnswer(r) + " for storage.")}
			})}
		}
		return []Step{Ask([]Question{{Question: "Which database should we use?", Choices: dbs}}, func(r Response) []Step {
			switch choice := firstAnswer(r); {
			case r.Plain():
				return plain()
			case slices.Contains(dbs, choice):
				return []Step{Text("Using " + choice + " for storage.")}
			default:
				return []Step{Text("Using " + choice + ".")}
			}
		})}
	})
	s.Agent(ns, "spike-nested").Reply(func(string) []Step {
		return []Step{
			Tool("transfer_to_agent", map[string]any{"agent_name": "specialist"}, nil),
			Ask([]Question{{Question: "Which region?", Choices: []string{"east", "west"}}}, func(r Response) []Step {
				return []Step{Text("Region " + firstAnswer(r) + " chosen.")}
			}),
		}
	})
	s.Agent(ns, "spike-approve").Reply(func(string) []Step {
		add := ToolRequest{Name: "add_numbers", Args: map[string]any{"a": 3, "b": 5}, Result: map[string]any{"output": "8"}}
		return []Step{RequestApproval([]ToolRequest{add}, func(r Response) []Step {
			switch {
			case r.Plain():
				return plain()
			case r.Approvals[0].Approved:
				return []Step{Text("I have added 3 and 5. The result is 8.")}
			default:
				return []Step{Text("Resumed after the tool result.")}
			}
		})}
	})
}

// plain is the spike's model answering a reply that is only text.
func plain() []Step { return []Step{Text("Saw a plain user message.")} }

// firstAnswer returns the first answer to the first question, or "" if there is none.
func firstAnswer(r Response) string {
	if len(r.Answers) > 0 && len(r.Answers[0]) > 0 {
		return r.Answers[0][0]
	}
	return ""
}
