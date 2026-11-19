package render

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// webhookLead starts the sentence before a webhook event's block.
const webhookLead = "The webhook event that triggered this"

// prEvent is a pull request's event as the webhook platform hands it on: an actor, id fields, a text field, and a field that did not
// resolve.
func prEvent() *chat.WebhookEvent {
	return &chat.WebhookEvent{Delivery: "d-1", Actor: "octocat", Fields: []chat.WebhookField{
		{Name: "number", Value: "42", ID: true, Present: true},
		{Name: "cloneURL", Value: "https://github.com/acme/widgets.git", ID: true, Present: true},
		{Name: "title", Value: "Retry <the> refresh", Present: true},
		{Name: "comment"},
	}}
}

// firstLine is the first line of s.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// webhookView is the view the trigger start gives a webhook event: the channel and the source, and no fields of its own.
func webhookView() v1alpha1.PromptView {
	return v1alpha1.PromptView{Channel: "acme/widgets", Source: "webhookEvent"}
}

// webhookTarget is a webhook event's block, as RenderWebhook appends it: the actor, an id field and a text field.
func webhookTarget() hostileTarget {
	return hostileTarget{
		name: "webhook block", prefix: untrustedDataPrefix, maxUnits: MaxUntrustedUnits, whole: true,
		elements: []string{"actor", "number", "comment"},
		places: []hostilePlace{
			{"actor", "actor", kindIdentifier, MaxFieldUnits},
			{"number", "number", kindIdentifier, MaxFieldUnits},
			{"comment", "comment", kindWindow, MaxFieldUnits},
		},
		build: func(t *testing.T, x []string) string {
			t.Helper()
			ev := &chat.WebhookEvent{Actor: x[0], Fields: []chat.WebhookField{
				{Name: "number", Value: x[1], ID: true, Present: true},
				{Name: "comment", Value: x[2], Present: true},
			}}
			got, err := newPrompt(t, "Review the pull request.").RenderWebhook(webhookView(), ev)
			if err != nil {
				t.Fatal(err)
			}
			_, rest, _ := strings.Cut(got, webhookLead)
			return webhookLead + rest
		},
	}
}

// webhookBlock is a webhook event's block that writes the elements, for checkSection.
func webhookBlock(elements ...string) hostileTarget {
	return hostileTarget{name: "webhook block", prefix: untrustedDataPrefix, maxUnits: MaxUntrustedUnits, whole: true, elements: elements}
}

// A webhook event's prompt: .Channel and the id fields take the .Key rule, so owner/repo and a URL render, and their values are the
// event's; the untrusted block holds the actor and then each field that has a value, id and text, in the connection's order, under
// its name, and a field that did not resolve is not there. Render, for a message, keeps the name shape and refuses the same channel.
func TestRenderWebhook(t *testing.T) {
	p := newPrompt(t, "Review #{{ .Fields.number }} of {{ .Channel }} from {{ .Fields.cloneURL }}.")
	view := webhookView()
	view.Fields = map[string]string{"number": "not the event's"} // the event's values are read, not the view's
	got, err := p.RenderWebhook(view, prEvent())
	if err != nil {
		t.Fatal(err)
	}
	if want := "Review #42 of acme/widgets from https://github.com/acme/widgets.git.\n\n" + webhookLead + " is below"; !strings.HasPrefix(got, want) {
		t.Errorf("the prompt starts %q; want %q", firstLine(got), want)
	}
	tag, body := checkSection(t, webhookBlock("actor", "number", "cloneURL", "title"), strings.TrimPrefix(got, "Review #42 of acme/widgets from https://github.com/acme/widgets.git.\n\n"))
	want := "<actor>octocat</actor>\n<number>42</number>\n<cloneURL>https://github.com/acme/widgets.git</cloneURL>\n<title>Retry &lt;the&gt; refresh</title>\n"
	if body != want {
		t.Errorf("the block (%s) holds\n%s\nwant\n%s", tag, body, want)
	}

	if _, err := p.Render(webhookView(), TriggerMessage{}); err == nil {
		t.Error("Render took the channel acme/widgets; want it refused by the name shape, as for a message")
	}
	bad := prEvent()
	bad.Fields[0].Value = "42; rm -rf /"
	var refused *RefusedError
	if _, err := p.RenderWebhook(webhookView(), bad); !errors.As(err, &refused) || refused.Ref != "Fields.number" || refused.Reason != ReasonCharacters {
		t.Errorf("RenderWebhook with a value outside the .Key rule = %v; want a refusal of .Fields.number", err)
	}
}

// D2, the prompt's half: a template that reads an Invalid id field refuses the render, with the reason invalid, and an id field that
// is absent reads empty. A field that is not an id field of the event, which resolution refuses, fails the render. No error holds a
// value of the event.
func TestRenderWebhookRefusesAnInvalidField(t *testing.T) {
	ev := prEvent()
	ev.Fields = append(ev.Fields, chat.WebhookField{Name: "sha", ID: true, Invalid: true}, chat.WebhookField{Name: "ref", ID: true})
	var refused *RefusedError
	_, err := newPrompt(t, "Review {{ .Fields.sha }}.").RenderWebhook(webhookView(), ev)
	if !errors.As(err, &refused) || refused.Ref != "Fields.sha" || refused.Reason != ReasonInvalid || refused.Reason.Code() != "invalid" {
		t.Fatalf("RenderWebhook = %v; want a refusal of .Fields.sha, invalid", err)
	}
	if got, err := newPrompt(t, "Review [{{ .Fields.ref }}].").RenderWebhook(webhookView(), ev); err != nil || !strings.HasPrefix(got, "Review [].") {
		t.Errorf("RenderWebhook of an absent id field = %q, %v; want it read empty", firstLine(got), err)
	}
	for _, tmpl := range []string{"{{ .Fields.title }}", "{{ .Fields.missing }}"} {
		_, err := newPrompt(t, tmpl).RenderWebhook(webhookView(), ev)
		if err == nil || errors.As(err, &refused) {
			t.Errorf("RenderWebhook(%s) = %v; want a render error", tmpl, err)
			continue
		}
		if strings.Contains(err.Error(), "Retry") || strings.Contains(err.Error(), "42") {
			t.Errorf("RenderWebhook(%s): the error %q holds a value of the event", tmpl, err)
		}
	}
	if strings.Contains(refused.Error(), "Retry") {
		t.Errorf("the refusal %q holds a value of the event", refused)
	}
}

// A field that does not fit is left out whole, never cut, and the block's last line names it: one over its own cap, and one that
// fits its cap but not the room that is left, while a later field that fits is still shown. Nothing in the block is cut, and the
// section is within its cap. When every field fits, there is no such line.
func TestRenderWebhookLeavesOutAFieldThatDoesNotFitWhole(t *testing.T) {
	long := strings.Repeat("a", MaxFieldUnits+1)   // over its own cap
	room := strings.Repeat("b", MaxFieldUnits-100) // within its cap
	ev := &chat.WebhookEvent{Actor: "octocat", Fields: []chat.WebhookField{
		{Name: "number", Value: "42", ID: true, Present: true},
		{Name: "body", Value: long, Present: true},
		{Name: "first", Value: room, Present: true},
		{Name: "second", Value: room, Present: true},
		{Name: "third", Value: room, Present: true}, // no room is left for it
		{Name: "action", Value: "opened", ID: true, Present: true},
	}}
	got, err := newPrompt(t, "Review.").RenderWebhook(webhookView(), ev)
	if err != nil {
		t.Fatal(err)
	}
	section := strings.TrimPrefix(got, "Review.\n\n")
	if chat.Units(section) > MaxUntrustedUnits {
		t.Errorf("the section is %d units; want at most %d", chat.Units(section), MaxUntrustedUnits)
	}
	_, body := checkSection(t, webhookBlock("actor", "number", "first", "second", "action"), section)
	if strings.Contains(body, chat.Truncated) || strings.Contains(body, "<body>") || strings.Contains(body, "<third>") {
		t.Errorf("the block cut a field, or holds one that does not fit:\n%.400s", body)
	}
	for _, name := range []string{"actor", "number", "first", "second", "action"} {
		if !strings.Contains(body, "<"+name+">") {
			t.Errorf("the block lacks %s, which fits", name)
		}
	}
	if !strings.HasSuffix(body, "</action>\n"+webhookLeftOut+" body, third.\n") {
		t.Errorf("the block ends %q; want the line that names body and third", body[max(len(body)-120, 0):])
	}

	all, err := newPrompt(t, "Review.").RenderWebhook(webhookView(), prEvent())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all, webhookLeftOut) {
		t.Errorf("a block that holds every field says one was left out:\n%s", all)
	}
}

// The whole policy, over sizes that put the room's end at each place: the section is within its cap whatever the sizes, every item it
// shows is whole, and the last line names exactly the items that it does not show, in their order.
func TestUntrustedBlockLeaveOutWhole(t *testing.T) {
	t.Parallel()
	const maxUnits = 2000
	names := []string{"one", "two", "three", "four"}
	for _, size := range sweep(1, 900, 37) {
		for _, small := range []int{1, 50, 400} {
			items := make([]untrustedItem, len(names))
			values := make([]string, len(names))
			for i, name := range names {
				values[i] = strings.Repeat(string(rune('a'+i)), []int{size, small, size, small}[i])
				items[i] = untrustedItem{Fields: []untrustedField{{Name: name, Value: values[i], Cap: 600}}}
			}
			b := untrustedBlock{Opening: "Notes", Plural: true, TagPrefix: "test-block-", MaxUnits: maxUnits, Whole: &wholePolicy{Lead: "Not shown:"}, Items: items}
			got, left := b.render()
			if chat.Units(got) > maxUnits {
				t.Fatalf("%d/%d: %d units for a cap of %d", size, small, chat.Units(got), maxUnits)
			}
			_, body := checkSection(t, hostileTarget{name: "whole", prefix: "test-block-", plural: true, maxUnits: maxUnits, many: true, elements: names}, got)
			var missing []string
			for i, name := range names {
				switch text := elemText(got, name); {
				case text == "" && strings.Contains(body, "<"+name+">"):
					t.Fatalf("%d/%d: %s is shown empty", size, small, name)
				case !strings.Contains(body, "<"+name+">"):
					missing = append(missing, name)
				case text != values[i]:
					t.Fatalf("%d/%d: %s is shown as %d units, cut", size, small, name, chat.Units(text))
				}
			}
			if left != len(missing) {
				t.Fatalf("%d/%d: left out %d, and %v are not shown", size, small, left, missing)
			}
			if line := "Not shown: " + strings.Join(missing, ", ") + ".\n"; (left > 0) != strings.HasSuffix(body, line) {
				t.Fatalf("%d/%d: %v left out, and the block ends %q", size, small, missing, body[max(len(body)-60, 0):])
			}
		}
	}
}
