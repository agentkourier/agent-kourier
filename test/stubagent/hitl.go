package stubagent

import (
	"maps"
	"strings"
)

// Question is one question of an ask-user pause.
type Question struct {
	Question string
	Choices  []string // none for a free-text question
	Multiple bool     // more than one choice may be picked
}

// Response is the user's reply to a pause, as the script that paused sees it.
type Response struct {
	Text string // the text of the user's message
}

// pause is what a paused task waits for.
type pause struct {
	id      string // the request id, which a reply repeats
	msgID   string // the id of the status message
	text    string // the status message text: all a client without the extension sees
	request obj    // the HITL request, sent only to a client that asked for the extension
	then    func(Response) []Step
	stamp   string
}

// message is the task's status message while it waits: the agent's request. A client without
// the extension gets it stripped down to the text (see rw.view).
func (p *pause) message(t *task) obj {
	return obj{
		"messageId":  p.msgID,
		"contextId":  t.sess.id,
		"taskId":     t.id,
		"role":       "ROLE_AGENT",
		"parts":      textParts(p.text),
		"extensions": []string{HITLExtension},
		"metadata":   obj{HITLExtension: p.request, "kagent.dev/a2a/timeline-position": p.stamp},
	}
}

// Ask ends the turn with an ask-user request, as kagent's ask_user tool does. The task goes
// INPUT_REQUIRED, and steps after Ask do not run; when the user answers, then returns the
// steps that do.
//
// The request reaches only a client that sent the A2A-Extensions header naming
// HITLExtension. Any other client sees the questions as the status text (WP-02, measured).
func Ask(questions []Question, then func(Response) []Step) Step { return askStep{questions, then} }

type askStep struct {
	questions []Question
	then      func(Response) []Step
}

func (a askStep) play(r *run) bool {
	var asked, requested []obj
	var texts []string
	for _, q := range a.questions {
		one := obj{"question": q.Question}
		if len(q.Choices) > 0 {
			one["choices"] = q.Choices
		}
		asked = append(asked, one)
		requested = append(requested, withMultiple(one, q.Multiple))
		texts = append(texts, q.Question)
	}
	call, id := r.s.newID("call"), r.s.newID("adk")
	if r.endText() &&
		r.data(obj{"id": call, "name": "ask_user", "args": obj{"questions": asked}}, "function_call") &&
		r.data(obj{"id": call, "name": "ask_user", "response": obj{"questions": asked, "status": "pending"}}, "function_response") {
		r.pause(&pause{
			id: id, msgID: r.s.newID("message"), text: strings.Join(texts, "\n"), then: a.then,
			request: obj{"type": "ask_user_request", "id": id, "questions": requested},
		})
	}
	return false // the turn continues, if the user answers, with the steps then returns
}

func withMultiple(question obj, multiple bool) obj {
	q := maps.Clone(question)
	q["multiple"] = multiple
	return q
}
