package interact

import (
	"slices"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// storable returns in as it is kept in the store: what the agent wrote, normalized and with the secrets taken
// out. The agent's hint and the text of each question go through redact.FreeText, which deletes the invisible
// characters before it matches, so that none can split a secret's name past the redaction; a tool's name and a
// sub-agent's likewise, and a tool's arguments through render.RedactArgs, the redaction of the status line that
// shows them, so that what is stored is never more than what is shown of it. in is not changed: the pause is answered
// from the original, and a question is posted from it. An approval's prompt is built from this form (promptOf), so that
// what is shown is never more than what is kept.
//
// What Agent Kourier sends back to the agent is not redacted, so what the answer is built from is kept exactly: the
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

// storedRequest is an interaction's request as the store keeps it (store.Interaction.Request): the pause, as storable makes it,
// and the tools whose call the reply shows on its card. It is the JSON of a dialect.Interaction with one key more, so a reader
// that needs only the pause decodes it as one, and a request stored before the key decodes as one with no tool on its card.
type storedRequest struct {
	dialect.Interaction
	// OnCard is the IDs (dialect.Tool.ID) of the tools of an approval whose call the reply the task paused in shows on its card
	// with the summary of the arguments that their ending would show (onCardOf). The ending names them without it.
	OnCard []string `json:"on_card,omitempty"`
}

// onCardOf is the IDs of the tools of the approval whose call the reply shows on its card, from cards (Pause.Cards): those
// whose card is of the tool's own call ID and shows the tool's own name and the summary of its own arguments, as the step of the
// call makes them (render.Tool). A card of another tool or of other arguments does not say what the ending would, and neither
// does one whose summary is empty or cut, which shows less than the arguments: the ending keeps them. A sub-agent's approval
// has none: its calls are not the reply's, and one may share an ID with a call that is. in is the pause as parsed, the one the
// card was made from.
func onCardOf(in dialect.Interaction, cards map[string]chat.ToolLine) []string {
	if in.Kind != dialect.ToolApproval || in.Nested != nil || len(cards) == 0 {
		return nil
	}
	var ids []string
	for _, t := range in.Tools {
		shown, ok := cards[t.CallID]
		if !ok || t.CallID == "" {
			continue
		}
		want := render.Tool(t.Name, t.Args)
		if shown.Name == want.Name && shown.Summary == want.Summary && want.Summary != "" && !strings.HasSuffix(want.Summary, chat.Truncated) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// markOnCard marks the tools of the prompt p, made from r in its order (toolsOf), whose call the reply shows on its card
// (chat.Tool.OnCard), and returns p.
func (r storedRequest) markOnCard(p chat.Prompt) chat.Prompt {
	for i, t := range r.Tools {
		if i < len(p.Tools) && slices.Contains(r.OnCard, t.ID) {
			p.Tools[i].OnCard = true
		}
	}
	return p
}
