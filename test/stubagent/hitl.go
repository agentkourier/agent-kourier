package stubagent

import (
	"encoding/json"
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
	Text      string     // the text of the user's message
	Answers   [][]string // ask-user answers, one list per question, exactly as sent
	Approvals []Approval // tool-approval decisions, in the order sent
}

// Plain reports whether the reply carries no answer or decision, only text. kagent takes such
// a reply as a new user turn and drops the request that was pending (WP-02, measured).
func (r Response) Plain() bool { return r.Answers == nil && r.Approvals == nil }

// pause is what a paused task waits for.
type pause struct {
	id      string // the request id, which a reply repeats
	msgID   string // the id of the status message
	text    string // the status message text: all a client without the extension sees
	request obj    // the HITL request, sent only to a client that asked for the extension
	decode  func(payload obj) (Response, *rpcError)
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
		// askuser/21: the call's args carry choices only when there are some, and multiple only
		// when true; the request always carries both.
		one := obj{"question": q.Question}
		if len(q.Choices) > 0 {
			one["choices"] = q.Choices
		}
		if q.Multiple {
			one["multiple"] = true
		}
		asked = append(asked, one)
		choices := q.Choices
		if choices == nil {
			choices = []string{}
		}
		requested = append(requested, obj{"question": q.Question, "choices": choices, "multiple": q.Multiple})
		texts = append(texts, q.Question)
	}
	call, id := r.s.newID("call"), r.s.newID("adk")
	if r.endText() &&
		r.data(obj{"id": call, "name": "ask_user", "args": obj{"questions": asked}}, "function_call") &&
		r.data(obj{"id": call, "name": "ask_user", "response": obj{"questions": asked, "status": "pending"}}, "function_response") {
		r.pause(&pause{
			id: id, msgID: r.s.newID("message"), text: strings.Join(texts, "; "), // askuser/18, 21
			request: obj{"type": "ask_user_request", "id": id, "questions": requested},
			decode:  decodeAnswers(id),
			then: func(resp Response) []Step {
				var steps []Step
				if !resp.Plain() {
					steps = append(steps, responseStep{call, "ask_user", answeredResult(a.questions, resp.Answers)})
				}
				if a.then != nil {
					steps = append(steps, a.then(resp)...)
				}
				return steps
			},
		})
	}
	return false // the turn continues, if the user answers, with the steps then returns
}

// missingAnswers is kagent's error for an ask-user reply it cannot correlate, or that has no answers.
const missingAnswers = "ask_user decision is missing approval correlation or answers"

// decodeAnswers reads the reply to the ask-user request id. kagent checks only that the reply
// repeats the request id and has at least one answer; it accepts too few answers, and answers
// that are not choices (WP-02, measured), and so does the stub.
// UNCONFIRMED: a reply whose payload has no type, or another type, is taken as plain text, and
// so is a payload sent without the extension header. The fixtures reply only with the header
// and with the right type.
func decodeAnswers(id string) func(obj) (Response, *rpcError) {
	return func(payload obj) (Response, *rpcError) {
		if payload["type"] != "ask_user_response" {
			return Response{}, nil
		}
		given, _ := payload["answers"].([]any)
		if payload["id"] != id || len(given) == 0 {
			return Response{}, internalError(missingAnswers)
		}
		answers := make([][]string, len(given))
		for i, g := range given {
			list, _ := dig(g, "answer").([]any)
			for _, v := range list {
				s, _ := v.(string)
				answers[i] = append(answers[i], s)
			}
		}
		return Response{Answers: answers}, nil
	}
}

// answeredResult is what kagent's ask_user call returns once answered: every question with its
// answers, none if it got none, as a JSON string.
func answeredResult(questions []Question, answers [][]string) obj {
	pairs := []obj{}
	for i, q := range questions {
		answer := []string{}
		if i < len(answers) {
			answer = append(answer, answers[i]...)
		}
		pairs = append(pairs, obj{"question": q.Question, "answer": answer})
	}
	result, _ := json.Marshal(pairs) // strings and lists of strings always marshal
	return obj{"result": string(result)}
}

// responseStep is the function_response artifact of a tool call whose result arrives after a
// pause.
type responseStep struct {
	call, name string
	response   obj
}

func (s responseStep) play(r *run) bool {
	return r.data(obj{"id": s.call, "name": s.name, "response": s.response}, "function_response")
}
