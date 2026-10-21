package stubagent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// gateName is what a gate name may be: ASCII letters, digits, "_" and "-". The prompt that names
// a gate, the endpoint that releases it and the e2e helper that makes names all use it.
const gateName = `[\w-]+`

var (
	gatePattern   = regexp.MustCompile(`gate:(` + gateName + `)`) // finds the gate a prompt names
	validGatePatt = regexp.MustCompile(`^` + gateName + `$`)
	// blockTag finds the name of the untrusted block Agent Kourier appends to a trigger's first prompt.
	blockTag = regexp.MustCompile(`untrusted-data-[0-9a-f]{12}`)
	// threadBlockOpen finds the opening line of the block of thread replies Agent Kourier appends to a mentioned turn in a
	// Binding whose chat.threadReplies is mention: the tag alone on its line. The sentence before the block names the tag
	// inline, so a match on the line is the block's own.
	threadBlockOpen = regexp.MustCompile(`(?m)^<(thread-replies-[0-9a-f]{12})>$`)
)

// answerNotReady is the text scenario-answer-fails refuses an answer with: a -32004 with it is what kagent answers a
// session that cannot take input (its dialect in internal/dialect/kagentv1 reads it as not ready, not busy).
const answerNotReady = "cannot accept work during a lifecycle operation"

// ValidGate reports whether name can name a gate. Gates stay open for the life of the server, so
// a name is used once: a reused name is already released.
func ValidGate(name string) bool { return validGatePatt.MatchString(name) }

// ScenarioAgents registers the agents that only the Phase 1 E2E scenarios need, in the namespace
// kagent, beside SpikeAgents:
//
//   - scenario-gated streams "Holding on gate G.", holds until Server.Release("G"), then streams
//     " Released." and finishes. G is the word after "gate:" in the prompt, unique per run; with
//     no gate in the prompt it finishes at once, so a reply that names none never wedges.
//   - scenario-memory says how many turns its context has had and quotes the earlier ones:
//     `Message 3. Earlier: "one", "two".`, or `Message 1. Earlier: none.` for a new context.
//   - scenario-ask asks the two questions of spike-ask's "two questions" (a multi-select and a
//     free-text one), then echoes exactly what it received, one entry per answered question:
//     `Answers: "PostgreSQL, MySQL" | "blue".`, an empty entry shown as (none).
//   - scenario-fails ends its task FAILED with no output, and a provider's masked-key error as its status
//     message ("Incorrect API key provided: sk-proj-...wxyz. HTTP 401"), which Agent Kourier must not show. A prompt
//     that says "empty" ends it COMPLETED with no output at all.
//   - scenario-tools says "Checking the service.", calls check_status(service="api"), which succeeds, says
//     "It is up, so I will restart it.", calls restart_service(service="api"), whose result is an error
//     ({"error": "restart refused: api is locked"}, the entry kagent reports a failed call with), and says
//     "The restart was refused." It never waits, so the whole turn ends in moments.
//   - scenario-context says which turn of its context it is, how many replies of the thread the turn carried and the tag of
//     the block that held them: "Turn 2. Replies: 1. Block: thread-replies-0123456789ab.", or "Block: none." for a turn
//     with no block. With "gate:G" in the prompt it then holds on the gate and ends with " Released.", as scenario-gated.
//   - scenario-answer-fails asks "Which service?" (free text), and the answer never gets a reply: with "fail gate:G"
//     in the prompt the turn the answer resumes holds on G, says nothing, then ends the task FAILED ("The model call failed after
//     the answer."); with "refuse gate:G" the answer's send waits on G, then is refused with -32004 "cannot accept
//     work during a lifecycle operation" (RefuseAnswer), which kagent's dialect reads as not ready, and the task
//     still waits for input. With no gate in the prompt it finishes at once, as scenario-gated.
//
// The scenarios and the agents that serve them in the cluster:
//
//	S1   scenario-gated   the first chunk is visible while the task is held, before the end;
//	                      release it through cmd/stubagent's /_control/release/{gate}. (spike-slow
//	                      takes 2.1 s at the 300 ms default tick, about the 2 s edit throttle, so
//	                      it shows the final text and at most one update)
//	S2   scenario-memory  "Message 3" with the earlier texts after an Agent Kourier restart proves the
//	                      contextId was kept; "Message 1" with none proves it was lost
//	S3   scenario-ask     "Answers: ... | (none)." means an incomplete answer reached the agent.
//	                      Neither the stub nor spike-ask rejects one (spike-ask says "Both
//	                      answered." to [[x], []]), so Agent Kourier's check is the only guard
//	S4   scenario-gated   mention with "gate:G1", reply with "gate:G2" while G1 is held, so the
//	                      reply is queued; release G1, then G2. Neither depends on timing
//	S5   test-agent       one instant chunk, so the kill lands between post and record
//	S11  spike-approve    "I have added 3 and 5..." would mean the tool ran; Agent Kourier's denial
//	                      gives "Resumed after the tool result."
//	S12  scenario-fails   the thread gets Agent Kourier's notice that the task failed (or finished with no
//	                      output), and none of the status message
//	S13  scenario-tools   the reply's tool steps: check_status done, restart_service an error, none left
//	                      in progress, and each text once
//	S14  scenario-alert   "Investigating: <the prompt's first line> Block: <the untrusted block's tag>." shows
//	                      that an alert's first turn is the template's output and the hashed block
//	S18  scenario-context "Turn N. Replies: K. Block: <tag|none>." shows that a mention-only Binding sent the unmentioned
//	                      replies since the last turn, once, in a block of their own
//	S26  scenario-answer-fails  the answer's turn shows the progress card while G is held, then fails or is
//	                      refused as not ready; release G through /_control/release/{gate}, as for S1
func ScenarioAgents(s *Server) {
	const ns = "kagent"
	s.Agent(ns, "scenario-alert").Reply(func(prompt string) []Step {
		first, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
		block := "none"
		if tag := blockTag.FindString(prompt); tag != "" {
			block = tag
		}
		return []Step{Text("Investigating: " + first + " Block: " + block + ".")}
	})
	s.Agent(ns, "scenario-context").ReplyWithHistory(func(prior []string, prompt string) []Step {
		tag, replies, said := "none", 0, prompt
		if all := threadBlockOpen.FindAllStringSubmatchIndex(prompt, -1); len(all) > 0 {
			open := all[len(all)-1] // the last: the person's own text comes before the block Agent Kourier appends
			tag, replies, said = prompt[open[2]:open[3]], strings.Count(prompt[open[1]:], "<reply>"), prompt[:open[0]]
		}
		steps := []Step{Text(fmt.Sprintf("Turn %d. Replies: %d. Block: %s.", len(prior)+1, replies, tag))}
		if m := gatePattern.FindStringSubmatch(said); m != nil { // a gate is the person's to name, not the replies'
			steps = append(steps, Hold(m[1]), Text(" Released."))
		}
		return steps
	})
	s.Agent(ns, "scenario-tools").Reply(func(string) []Step {
		service := map[string]any{"service": "api"}
		return []Step{
			Text("Checking the service."),
			Tool("check_status", service, map[string]any{"output": "api is up"}),
			Text("It is up, so I will restart it."),
			Tool("restart_service", service, map[string]any{"error": "restart refused: api is locked"}),
			Text("The restart was refused."),
		}
	})
	s.Agent(ns, "scenario-gated").Reply(func(prompt string) []Step {
		m := gatePattern.FindStringSubmatch(prompt)
		if m == nil {
			return []Step{Text("No gate named, not holding.")}
		}
		return []Step{Text("Holding on gate " + m[1] + "."), Hold(m[1]), Text(" Released.")}
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
	s.Agent(ns, "scenario-fails").Reply(func(prompt string) []Step {
		if strings.Contains(prompt, "empty") {
			return []Step{Text()}
		}
		return []Step{Fail("Incorrect API key provided: sk-proj-********************wxyz. HTTP 401")}
	})
	s.Agent(ns, "scenario-answer-fails").Reply(func(prompt string) []Step {
		m := gatePattern.FindStringSubmatch(prompt)
		if m == nil {
			return []Step{Text("No gate named, not asking.")}
		}
		refuse := strings.Contains(prompt, "refuse")
		return []Step{Ask([]Question{{Question: "Which service?"}}, func(r Response) []Step {
			switch {
			case r.Plain():
				return plain()
			case refuse:
				return []Step{RefuseAnswer(m[1], answerNotReady)}
			}
			return []Step{Hold(m[1]), Fail("The model call failed after the answer.")}
		})}
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
