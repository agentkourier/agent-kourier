package chattest

import "github.com/agentkourier/agent-kourier/internal/chat"

// Field is a piece of text in an Outgoing and the place it sits in.
type Field struct {
	Text  string
	Where chat.Where
}

// Fields returns every piece of text in o, with its place: the fields an adapter must clean, cut and
// neutralise for its platform, and the ones an adapter's tests look for in what it sent. A user ID
// (Ending.By) is listed too, since it is text until the adapter has checked its shape.
func Fields(o chat.Outgoing) []Field {
	var out []Field
	add := func(s string, w chat.Where) { out = append(out, Field{s, w}) }
	switch {
	case o.Message != nil:
		out = append(out, MessageFields(*o.Message)...)
	case o.Prompt != nil:
		out = append(out, promptFields(*o.Prompt)...)
	case o.Ending != nil:
		out = append(out, promptFields(o.Ending.Prompt)...)
		add(o.Ending.By, chat.WhereUser)
		for _, a := range o.Ending.Answers {
			for _, s := range a {
				add(s, chat.WhereAnswer)
			}
		}
	}
	return out
}

// MessageFields returns every piece of text in m, with its place.
func MessageFields(m chat.Message) []Field {
	out := []Field{{m.Fallback, chat.WhereFallback}}
	add := func(s string, w chat.Where) { out = append(out, Field{s, w}) }
	for _, p := range m.Parts {
		switch p := p.(type) {
		case chat.Header:
			add(p.Text, chat.WhereHeader)
		case chat.Body:
			add(p.Markdown, chat.WhereBody)
		case chat.Context:
			add(p.Text, chat.WhereContext)
		case chat.Note:
			add(p.Text, chat.WhereNote)
		case chat.ToolLine:
			add(p.Name, chat.WhereToolName)
			add(p.Summary, chat.WhereToolArg)
		case chat.Buttons:
			for _, b := range p.Buttons {
				add(b.Label, chat.WhereButton)
			}
		}
	}
	return out
}

func promptFields(p chat.Prompt) []Field {
	out := []Field{{p.Hint, chat.WhereHint}, {p.Subagent, chat.WhereSubagent}}
	for _, q := range p.Questions {
		out = append(out, Field{q.Text, chat.WhereQuestion})
		for _, c := range q.Choices {
			out = append(out, Field{c, chat.WhereChoice})
		}
	}
	return out
}
