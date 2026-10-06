package interact

import (
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// storable returns in as it is kept in the store: what the agent wrote, normalized and with the secrets taken
// out. The agent's hint and the text of each question go through redact.FreeText, which deletes the invisible
// characters before it matches, so that none can split a secret's name past the redaction; a tool's name and a
// sub-agent's likewise, and a tool's arguments through render.RedactArgs, the redaction of the status line that
// shows them, so that what is stored is never more than what is shown of it. in is not changed: the pause is posted, and answered, from the original.
//
// What courier sends back to the agent is not redacted, so what the answer is built from is kept exactly: the
// request and tool IDs, and the choices of a question (an answer picks one by its index, and the agent
// receives the stored choice). A secret that is itself a choice is therefore stored, until the interaction is
// cleared (store.Interactions.ClearSettledInteractions). Nothing the dialect builds an answer from is among what
// is changed here (kagentv1's askUserAnswer reads the questions' number and choices, and toolApprovalAnswer
// the tools' IDs).
func storable(in dialect.Interaction) dialect.Interaction {
	in.Hint = redact.FreeText(in.Hint)
	if in.Questions != nil {
		qs := make([]dialect.Question, len(in.Questions))
		for i, q := range in.Questions {
			q.Text = redact.FreeText(q.Text)
			qs[i] = q
		}
		in.Questions = qs
	}
	if in.Tools != nil {
		ts := make([]dialect.Tool, len(in.Tools))
		for i, t := range in.Tools {
			t.Name = redact.FreeText(t.Name)
			t.Args = render.RedactArgs(t.Args)
			ts[i] = t
		}
		in.Tools = ts
	}
	if in.Nested != nil {
		n := *in.Nested
		n.Subagent = redact.FreeText(n.Subagent)
		in.Nested = &n
	}
	return in
}
