package chat_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The stored form of an Outgoing is version 1 of the outbox's payload. Rows written by this code are read by the
// next release, so the wire form is pinned here, one row of each kind, byte for byte: a field renamed, added without
// omitempty, or reordered fails this test, and after a release any such change is version 2, with a reader for 1.
func TestTheStoredV1FormIsPinned(t *testing.T) {
	deadline := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	prompt := chat.Prompt{ID: "ix-1", Hint: "Which?", Deadline: deadline, Subagent: "sre", Questions: []chat.Question{
		{Text: "Env?", Choices: []string{"prod", "dev"}, Multiple: true},
		{Text: "Why?"},
	}}
	for name, tc := range map[string]struct {
		out  chat.Outgoing
		want string
	}{
		"message": {chat.Say(chat.Message{Fallback: "fb", Parts: []chat.Part{
			chat.Header{Text: "h"}, chat.Body{Markdown: "*b*"}, chat.Context{Text: "c"}, chat.Note{Text: "n"},
			chat.ToolLine{Name: "kubectl", Summary: "ns=prod"}, chat.Mention{User: "U1", Text: "said"},
			chat.Buttons{Buttons: []chat.Button{{ID: "go", Label: "Go", Value: "ix-1", Primary: true}, {ID: "no", Label: "No", Value: "ix-1"}}},
		}}),
			`{"v":1,"message":{"fallback":"fb","parts":[{"kind":"header","text":"h"},{"kind":"body","markdown":"*b*"},` +
				`{"kind":"context","text":"c"},{"kind":"note","text":"n"},{"kind":"tool","name":"kubectl","summary":"ns=prod"},` +
				`{"kind":"mention","text":"said","user":"U1"},` +
				`{"kind":"buttons","buttons":[{"id":"go","label":"Go","value":"ix-1","primary":true},{"id":"no","label":"No","value":"ix-1"}]}]}}`},
		"prompt": {chat.Ask(prompt),
			`{"v":1,"prompt":{"id":"ix-1","hint":"Which?","questions":[{"text":"Env?","choices":["prod","dev"],"multiple":true},` +
				`{"text":"Why?"}],"deadline":"2026-10-02T12:00:00Z","subagent":"sre"}}`},
		"prompt with only what it needs": {chat.Ask(chat.Prompt{ID: "ix-2", Questions: []chat.Question{{Text: "q"}}}),
			`{"v":1,"prompt":{"id":"ix-2","questions":[{"text":"q"}]}}`},
		"ending": {chat.End(chat.Ending{Outcome: chat.Answered, By: "U1", At: deadline.Add(5 * time.Minute), Prompt: prompt, Answers: [][]string{{"prod", "dev"}, {"(answered)"}}}),
			`{"v":1,"ending":{"outcome":"answered","by":"U1","at":"2026-10-02T12:05:00Z","prompt":{"id":"ix-1","hint":"Which?",` +
				`"questions":[{"text":"Env?","choices":["prod","dev"],"multiple":true},{"text":"Why?"}],"deadline":"2026-10-02T12:00:00Z","subagent":"sre"},` +
				`"answers":[["prod","dev"],["(answered)"]]}}`},
		"ending that timed out": {chat.End(chat.Ending{Outcome: chat.TimedOut, Prompt: chat.Prompt{Questions: []chat.Question{{Text: "q"}}}}),
			`{"v":1,"ending":{"outcome":"timed_out","prompt":{"id":"","questions":[{"text":"q"}]}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(tc.out)
			if err != nil || string(got) != tc.want {
				t.Fatalf("stored form\n%s\nwant\n%s\n(%v)", got, tc.want, err)
			}
			var back chat.Outgoing
			if err := json.Unmarshal([]byte(tc.want), &back); err != nil || !reflect.DeepEqual(back, tc.out) {
				t.Errorf("reading the pinned row gave %+v, %v; want %+v", back, err, tc.out)
			}
		})
	}
}
