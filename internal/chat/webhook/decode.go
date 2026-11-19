package webhook

import (
	"errors"
	"net/http"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// result is how a request ended: its outcome and reason, the labels of agentkourier_webhook_requests_total. The zero result is an
// event still to hand to the Sink.
type result struct{ outcome, reason string }

// decode maps a verified request onto the event it carries, steps 2 to 5 of the package comment: it returns the event, whose ID is id
// and whose delivery is delivery, and the zero result, or the result of the step that ended it.
func (p *Platform) decode(h http.Header, body []byte, id, delivery string) (chat.Inbound, result) {
	vals, err := extract(body, p.ptrs)
	switch {
	case errors.Is(err, errDepth):
		return chat.Inbound{}, result{outcomeMalformed, reasonDepth}
	case errors.Is(err, errDuplicateKey):
		return chat.Inbound{}, result{outcomeMalformed, reasonDuplicateKey}
	case err != nil:
		return chat.Inbound{}, result{outcomeMalformed, reasonSchema}
	}
	channel, ok := p.value, true
	if len(p.channel) > 0 {
		channel, ok = firstOf(vals, p.channel)
	}
	if !ok || !p.channelShape(channel) {
		return chat.Inbound{}, result{outcomeUnmapped, reasonNoChannel}
	}
	var actor string
	if !p.none {
		resolved := false
		for _, ptr := range p.actors {
			v, ok := vals[ptr.raw]
			if !ok {
				continue
			}
			if !resolved {
				actor, resolved = v.text, true
			}
			if equalFoldASCII(v.text, p.identity) {
				return chat.Inbound{}, result{outcomeSelf, reasonNone}
			}
		}
		if !resolved && !p.keep {
			return chat.Inbound{}, result{outcomeIgnored, reasonNoActor}
		}
	}
	ev := &chat.WebhookEvent{Delivery: delivery, Actor: actor}
	for _, f := range p.fields {
		ev.Fields = append(ev.Fields, f.resolve(h, vals))
	}
	var thread string
	if p.thread >= 0 {
		f := ev.Fields[p.thread]
		switch {
		case f.Invalid:
			return chat.Inbound{}, result{outcomeUnmapped, reasonBadID}
		case f.Value == "":
			return chat.Inbound{}, result{outcomeUnmapped, reasonNoThread}
		}
		// chat.MessageID and the session's notice keys join the channel and the thread with ':', so that a thread with one could
		// collide with another event's key.
		if thread = p.prefix + f.Value; strings.Contains(thread, ":") {
			return chat.Inbound{}, result{outcomeUnmapped, reasonBadID}
		}
	}
	return chat.Inbound{At: p.RefAt(channel, thread, id), User: actor, Webhook: ev}, result{}
}

// firstOf returns the text of the first of ptrs that resolves.
func firstOf(vals map[string]value, ptrs []pointer) (string, bool) {
	for _, ptr := range ptrs {
		if v, ok := vals[ptr.raw]; ok {
			return v.text, true
		}
	}
	return "", false
}

// resolve returns the field's state in a request: the first of its values that resolves (each value of its header, or each of its
// pointers that resolves, in order) and, for an id field, passes idValue. An id field that has values and none that passes is
// Invalid.
func (f field) resolve(h http.Header, vals map[string]value) chat.WebhookField {
	var candidates []string
	if f.header != "" {
		candidates = h.Values(f.header)
	}
	for _, ptr := range f.from {
		if v, ok := vals[ptr.raw]; ok {
			candidates = append(candidates, v.text)
		}
	}
	out := chat.WebhookField{Name: f.name, ID: f.id}
	for _, v := range candidates {
		if f.id && !idValue(v) {
			out.Invalid = true
			continue
		}
		return chat.WebhookField{Name: f.name, Value: v, ID: f.id, Present: true}
	}
	return out
}

// idValue says whether s passes the id check, the prompt renderer's rule of a key: at most maxIDValue bytes of ASCII letters, digits
// and "_.-:/".
func idValue(s string) bool {
	if len(s) > maxIDValue {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("_.-:/", rune(c)) {
			return false
		}
	}
	return true
}

// deliveryID returns the request's delivery ID: the first value of the connection's delivery header, if it is not empty and passes
// idValue, or "".
func (p *Platform) deliveryID(h http.Header) string {
	if p.delivery == "" {
		return ""
	}
	if v := h.Get(p.delivery); v != "" && idValue(v) {
		return v
	}
	return ""
}

// equalFoldASCII compares a and b with ASCII letters folded and nothing else: a letter that only looks Latin is not equal.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
