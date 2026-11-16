package kourierv1

import (
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// markdownEscaper puts a backslash before each character that can open CommonMark's links, images, link reference definitions,
// autolinks and raw HTML, and before the backslash itself, so that a receiver's CommonMark shows the characters as they are. Emphasis,
// code and lists are left to render.
var markdownEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, `<`, `\<`)

// inert returns a copy of o to send: every literal field cleaned, on one line where its place is one, and cut to maxText (chat.Literal);
// the body's markdown cleaned, cut, then escaped (markdownEscaper); a user ID kept only if it has the kourier-v1 shape; and the Steps
// parts dropped, since the profile has no progress steps. The caller's o is not changed.
func (p *Profile) inert(o chat.Outgoing) chat.Outgoing {
	out := chat.Outgoing{V: o.V}
	switch {
	case o.Message != nil:
		m := p.message(*o.Message)
		out.Message = &m
	case o.Prompt != nil:
		pr := p.prompt(*o.Prompt)
		out.Prompt = &pr
	case o.Ending != nil:
		e := p.ending(*o.Ending)
		out.Ending = &e
	}
	return out
}

func (p *Profile) message(m chat.Message) chat.Message {
	out := chat.Message{Fallback: p.literal(m.Fallback, chat.WhereFallback)}
	for _, part := range m.Parts {
		switch part := part.(type) {
		case chat.Header:
			out.Parts = append(out.Parts, chat.Header{Text: p.literal(part.Text, chat.WhereHeader)})
		case chat.Body:
			out.Parts = append(out.Parts, chat.Body{Markdown: markdownEscaper.Replace(p.literal(part.Markdown, chat.WhereBody))})
		case chat.Context:
			out.Parts = append(out.Parts, chat.Context{Text: p.literal(part.Text, chat.WhereContext)})
		case chat.Note:
			out.Parts = append(out.Parts, chat.Note{Text: p.literal(part.Text, chat.WhereNote)})
		case chat.ToolLine:
			out.Parts = append(out.Parts, chat.ToolLine{Name: p.literal(part.Name, chat.WhereToolName),
				Summary: p.literal(part.Summary, chat.WhereToolArg), State: part.State})
		case chat.Mention:
			out.Parts = append(out.Parts, chat.Mention{User: p.user(part.User), Text: p.literal(part.Text, chat.WhereContext)})
		case chat.Buttons:
			bs := make([]chat.Button, len(part.Buttons))
			for i, b := range part.Buttons {
				b.Label = p.literal(b.Label, chat.WhereButton)
				bs[i] = b
			}
			out.Parts = append(out.Parts, chat.Buttons{Buttons: bs})
		case chat.Steps:
			// The profile has no progress steps (Caps.Steps false): a platform without them shows nothing of the part.
		}
	}
	return out
}

func (p *Profile) prompt(pr chat.Prompt) chat.Prompt {
	out := pr
	out.Hint = p.literal(pr.Hint, chat.WhereHint)
	out.Subagent = p.literal(pr.Subagent, chat.WhereSubagent)
	out.Questions = nil
	for _, q := range pr.Questions {
		nq := chat.Question{Text: p.literal(q.Text, chat.WhereQuestion), Multiple: q.Multiple}
		for _, c := range q.Choices {
			nq.Choices = append(nq.Choices, p.literal(c, chat.WhereChoice))
		}
		out.Questions = append(out.Questions, nq)
	}
	// A prompt of tools is refused before it is encoded (the profile takes no tool approvals); were one sent, it would be cleaned whole.
	out.Tools = nil
	for _, t := range pr.Tools {
		out.Tools = append(out.Tools, chat.Tool{Name: chat.Clean(t.Name, true), Args: chat.Clean(t.Args, false)})
	}
	return out
}

func (p *Profile) ending(e chat.Ending) chat.Ending {
	out := e
	out.By = p.user(e.By)
	out.Prompt = p.prompt(e.Prompt)
	out.Answers = nil
	for _, a := range e.Answers {
		na := make([]string, len(a))
		for i, s := range a {
			na[i] = p.literal(s, chat.WhereAnswer)
		}
		out.Answers = append(out.Answers, na)
	}
	out.Verdicts = append([]chat.ToolVerdict(nil), e.Verdicts...)
	return out
}

// literal is s as chat.Literal shows it at w on the profile.
func (p *Profile) literal(s string, w chat.Where) string { return chat.Literal(s, w, p.Caps()) }

// user is id if it has the shape of a kourier-v1 user ID, and "" otherwise: the receiver shows "someone" for it.
func (p *Profile) user(id string) string {
	if p.shapes.User.Matches(id) {
		return id
	}
	return ""
}
