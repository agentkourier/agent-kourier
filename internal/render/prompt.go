package render

import (
	"fmt"
	"slices"
	"strings"
	"text/template"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The first prompt of a chat trigger (spec 8.2, R2). It has
// two parts. The instruction part is the output of the trigger's promptTemplate, which is written
// by the operator but reads values taken out of an attacker's message; each value it reads must
// pass a strict character check (promptcheck.go) or the render is refused. After it Agent Kourier
// always appends the whole matched message, in an untrusted-data block it writes itself, so a
// template can neither move the message nor leave it out. Neither part may be longer than a cap.
//
// The block is
//
//	<untrusted-data-0123456789ab>
//	<title>...</title>
//	<text>...</text>
//	</untrusted-data-0123456789ab>
//
// with the fields that the message has, in the order title, text, body, fallback,
// link and footer, named as the trigger's match and extract name them. It is built by the untrusted block module
// (untrusted.go), which writes the sentence before it (that the block is data and not instructions, and where the tag is),
// the tag, and the escaping. This file says only what the block holds: the message's fields, each read as follows. A
// field is normalized and redacted (redact.FreeTextCut: the invisible characters deleted, the line breaks made "\n", the
// secrets taken out, in the first 12000 bytes, cut at a character), the one-line title and link put on one line
// (chat.Clean), and the footer, which is a line of identifiers such as "agent-kourier-key: ns/alertname", read as identifier
// text (redact.IdentifierText: the tokens taken out, a "name: value" pair kept). The cost of the escaping is that Slack's
// own markup, "<url|text>" and "&amp;", is escaped a second time.
//
// An event of a webhook connection has no such fields: its block holds the event's actor and its named fields, in the
// connection's order (RenderWebhook, untrustedWebhook).

// Caps on a prompt, in UTF-16 units as chat.Truncate counts them. What is cut ends in
// chat.Truncated, so the agent can see it.
const (
	// MaxInstructionUnits caps the instruction part, the template's output, which holds only
	// values of at most 512 bytes each. A longer one is the template's fault and is cut.
	MaxInstructionUnits = 4000
	// MaxFieldUnits caps one field of the message, as escaped inside its tags and with the mark
	// of a cut.
	MaxFieldUnits = 3000
	// MaxUntrustedUnits caps the untrusted section: the sentence before the block, the block, its
	// tags and the fields in it. Fields that do not fit are left out, and a last line says so.
	MaxUntrustedUnits = 8000
	// MaxPromptUnits caps the prompt: the instruction part, a blank line, and the untrusted section.
	MaxPromptUnits = MaxInstructionUnits + len("\n\n") + MaxUntrustedUnits
)

// TriggerMessage is the matched message, as the neutral fields the trigger's regexes read
// (v1alpha1.MessageField). Every one is untrusted, and all of them are put in the block, so the
// agent sees the message whole whatever the template reads of it. An empty field is left out.
type TriggerMessage struct {
	Text     string
	Title    string
	Link     string
	Fallback string
	Footer   string
	Body     string
}

// MessageOf returns the untrusted block's message for m: its text and the fields of its bot's attachment, each read
// through chat.Inbound.Field, the accessor the trigger's regexes use. It is the only place an Inbound becomes a
// TriggerMessage; a test fills every field of the Inbound and checks each reaches the block, so a field added to
// the Inbound later cannot be left out of what the agent sees.
func MessageOf(m chat.Inbound) TriggerMessage {
	return TriggerMessage{
		Text: m.Field(v1alpha1.FieldText), Title: m.Field(v1alpha1.FieldTitle), Link: m.Field(v1alpha1.FieldLink),
		Fallback: m.Field(v1alpha1.FieldFallback), Footer: m.Field(v1alpha1.FieldFooter), Body: m.Field(v1alpha1.FieldBody),
	}
}

// Prompt is a parsed promptTemplate, ready to render the first prompt of a trigger. Parse it once
// at load; it is safe for concurrent use.
type Prompt struct {
	tmpl *template.Template
	refs v1alpha1.PromptRefs
}

// NewPrompt parses text as a trigger's promptTemplate with v1alpha1.ParsePromptTemplate, the parse
// the load-time check uses, and finds with v1alpha1.TemplateRefs what it reads of a PromptView.
// It fails on a template that does not parse and on one that TemplateRefs refuses: one that reads
// the view in a way the check of its values cannot cover, or that could loop, recurse or grow
// without bound. It runs nothing.
func NewPrompt(name, text string) (*Prompt, error) {
	tmpl, err := v1alpha1.ParsePromptTemplate(name, text)
	if err != nil {
		return nil, fmt.Errorf("parse prompt template: %w", err)
	}
	refs, err := v1alpha1.TemplateRefs(tmpl)
	if err != nil {
		return nil, fmt.Errorf("check prompt template: %w", err)
	}
	return &Prompt{tmpl: tmpl, refs: refs}, nil
}

// Render returns the first prompt: the template's output over view, cut to MaxInstructionUnits,
// a blank line, and the untrusted block of msg. Each value of view that the template reads must
// pass its check (see PromptView in api/v1alpha1), and the first that does not, in the order
// Channel, Key, Link, Source, State and then the entries by name, gives a *RefusedError and no
// prompt. A value the template does not read is not checked, and it is not shown to the
// template either. Render also fails if the template reads an entry that view lacks, fails to
// run, or writes more than v1alpha1.MaxPromptOutput bytes. These are template faults, not
// hostile text: a slice longer than a real value passes the load check, whose placeholders are
// long, and fails here, so the caller treats a render error as it does a refusal and starts no
// turn.
func (p *Prompt) Render(view v1alpha1.PromptView, msg TriggerMessage) (string, error) {
	return p.render(view, false, func() string { return untrustedSection(msg) })
}

// RenderWebhook is Render for an event of a webhook connection (spec 8.2, "Webhook events"). The template reads .Channel and the
// connection's id fields (.Fields.<name>), which resolution holds it to, and both are held to the .Key rule (ASCII letters, digits
// and "_.-:/", at most 128 bytes) in place of the name shape, so that a repository's owner/name and a URL render. The values of
// .Fields are ev's, not view's: an id field the event has no value for is empty. One the template reads that is Invalid (it had
// values and none passed the id check) refuses the render, with ReasonInvalid, so that its absence is never shown for a value; one
// that is not an id field of ev, which resolution refuses, fails it. The untrusted block holds the event's actor and then each field
// that has a value, id and text, in the connection's order, under its name; a field that does not fit is left out whole, never
// cut, and a last line names it (untrustedWebhook). No error holds a value of the event.
func (p *Prompt) RenderWebhook(view v1alpha1.PromptView, ev *chat.WebhookEvent) (string, error) {
	var fields []chat.WebhookField
	if ev != nil {
		fields = ev.Fields
	}
	view.Fields = make(map[string]string, len(p.refs.Fields))
	for _, name := range p.refs.Fields {
		i := slices.IndexFunc(fields, func(f chat.WebhookField) bool { return f.Name == name })
		switch {
		case i < 0 || !fields[i].ID:
			return "", fmt.Errorf("prompt template reads .Fields.%s, which is not an id field of the event", name)
		case fields[i].Invalid:
			return "", &RefusedError{Ref: "Fields." + name, Reason: ReasonInvalid}
		}
		view.Fields[name] = fields[i].Value
	}
	return p.render(view, true, func() string { return untrustedWebhook(ev) })
}

// render is Render and RenderWebhook: the template's output over the checked view, cut, a blank line, and the untrusted section that
// section writes, once the view has passed. wide holds .Channel and .Fields to the .Key rule.
func (p *Prompt) render(view v1alpha1.PromptView, wide bool, section func() string) (string, error) {
	safe, err := p.safeView(view, wide)
	if err != nil {
		return "", err
	}
	out, err := v1alpha1.ExecutePrompt(p.tmpl, safe)
	if err != nil {
		return "", fmt.Errorf("render prompt template: %w", err)
	}
	instruction := chat.Truncate(strings.TrimSpace(out), MaxInstructionUnits)
	if instruction == "" {
		return section(), nil
	}
	return instruction + "\n\n" + section(), nil
}

// safeView checks the values of view that the template reads, and returns a view that holds
// only those. So whatever the template does with the view, an unchecked value is not in it. wide
// holds .Channel and the .Fields values to the .Key rule, for an event of a webhook connection.
func (p *Prompt) safeView(view v1alpha1.PromptView, wide bool) (v1alpha1.PromptView, error) {
	idRule := checkName
	if wide {
		idRule = checkKey
	}
	var safe v1alpha1.PromptView
	for _, name := range p.refs.View {
		var src string
		var dst *string
		rule := checkName
		switch name {
		case "Channel":
			src, dst, rule = view.Channel, &safe.Channel, idRule
		case "State":
			src, dst = view.State, &safe.State
		case "Key":
			src, dst, rule = view.Key, &safe.Key, checkKey
		case "Link":
			src, dst, rule = view.Link, &safe.Link, checkLink
		case "Source":
			src, dst = view.Source, &safe.Source
		default: // a field added to PromptView without a rule: refuse rather than pass it unchecked
			return v1alpha1.PromptView{}, fmt.Errorf("prompt view field %s has no check", name)
		}
		if err := check(name, src, rule); err != nil {
			return v1alpha1.PromptView{}, err
		}
		*dst = src
	}
	safe.Fields = make(map[string]string, len(p.refs.Fields))
	for _, name := range p.refs.Fields {
		value, ok := view.Fields[name]
		if !ok {
			return v1alpha1.PromptView{}, fmt.Errorf("prompt template reads .Fields.%s, which the view lacks", name)
		}
		if err := check("Fields."+name, value, idRule); err != nil {
			return v1alpha1.PromptView{}, err
		}
		safe.Fields[name] = value
	}
	return safe, nil
}

// alertWindowBytes is how much of a field is read for secrets: four times MaxFieldUnits, so that the 3000 units a field
// shows (never more than 9000 bytes of text) lie whole inside it, with room after them for a secret that runs
// across the end of what is shown. A field longer than it is cut at a character, not at white space, so that a body
// with none (Japanese) or a long link keeps its start.
const alertWindowBytes = 4 * MaxFieldUnits

// untrustedDataPrefix starts the name of the alert block's tag; 12 hex digits follow.
const untrustedDataPrefix = "untrusted-data-"

// alertItem is one field of the message as the block shows it: an item of its own, in an element named by the field,
// and read through a window (kindWindow), and, for a field that is one line (line), put on one. The footer is
// identifier text instead (kindIdentifier): the free-text rules would read its "agent-kourier-key: X" as a pair named by the
// secret word "key" and take the key out, and the key is what the footer is there for. A field's name is one of Agent Kourier's
// constants, never the message's. The agent is sent the cleaned field and nothing else of it, so a secret that an alert or
// a person wrote into one does not reach the model's provider.
func alertItem(name v1alpha1.MessageField, value string, kind fieldKind, line bool) untrustedItem {
	return untrustedItem{Fields: []untrustedField{{
		Name: string(name), Value: value, Kind: kind, OneLine: line, Cap: MaxFieldUnits, Window: alertWindowBytes,
	}}}
}

// untrustedSection returns the sentence and the block for msg, in at most MaxUntrustedUnits: each field of the
// message that has something in it, in the order title, text, body, fallback, link and footer. A block
// with no field is written empty. Fields that do not fit are left out, and a last line says so.
func untrustedSection(msg TriggerMessage) string {
	section, _ := untrustedBlock{
		Opening:   "The message that triggered this",
		TagPrefix: untrustedDataPrefix,
		MaxUnits:  MaxUntrustedUnits,
		Items: []untrustedItem{
			alertItem(v1alpha1.FieldTitle, msg.Title, kindWindow, true),
			alertItem(v1alpha1.FieldText, msg.Text, kindWindow, false),
			alertItem(v1alpha1.FieldBody, msg.Body, kindWindow, false),
			alertItem(v1alpha1.FieldFallback, msg.Fallback, kindWindow, false),
			alertItem(v1alpha1.FieldLink, msg.Link, kindWindow, true),
			alertItem(v1alpha1.FieldFooter, msg.Footer, kindIdentifier, false),
		},
	}.render()
	return section
}

// webhookLeftOut starts the last line of a webhook event's block that left fields out, before their names.
const webhookLeftOut = "Left out, too long to show here:"

// untrustedWebhook returns the sentence and the block for a webhook event, in at most MaxUntrustedUnits: its actor, then each field
// that has a value, in the connection's order, under its name, an id field read as identifier text and a text field through a window
// as a message's body is, each in at most MaxFieldUnits. A field that is Invalid or absent has no value and is not there. A field that
// does not fit, in its own cap or in the room that is left, is left out whole, and a last line names it (wholePolicy): a part of a
// comment could say what the whole does not. The actor is named actor unless the connection has a field of that name, which is shown
// in its place. A field's name is the connection's (a lower-case letter, then letters and digits), never the event's.
func untrustedWebhook(ev *chat.WebhookEvent) string {
	var items []untrustedItem
	if ev != nil {
		if ev.Actor != "" && !slices.ContainsFunc(ev.Fields, func(f chat.WebhookField) bool { return f.Name == "actor" }) {
			items = append(items, untrustedItem{Fields: []untrustedField{{Name: "actor", Value: ev.Actor, Kind: kindIdentifier, Cap: MaxFieldUnits}}})
		}
		for _, f := range ev.Fields {
			if !f.Present {
				continue
			}
			field := untrustedField{Name: f.Name, Value: f.Value, Kind: kindWindow, Cap: MaxFieldUnits, Window: alertWindowBytes}
			if f.ID {
				field.Kind = kindIdentifier
			}
			items = append(items, untrustedItem{Fields: []untrustedField{field}})
		}
	}
	section, _ := untrustedBlock{
		Opening:   "The webhook event that triggered this",
		TagPrefix: untrustedDataPrefix,
		MaxUnits:  MaxUntrustedUnits,
		Whole:     &wholePolicy{Lead: webhookLeftOut},
		Items:     items,
	}.render()
	return section
}
