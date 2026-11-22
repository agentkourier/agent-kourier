package agent

import (
	"reflect"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The task an answer goes on with holds the paused turn's text artifacts, and its final snapshot repeats them (kagent's
// and the CMA add-on's both do). That text is the paused turn's, shown in its message: the answer's send must not deliver
// it again, or the answer's message shows it after the answer's own text, where it was first seen by this send.
func TestAnAnswerDoesNotRepeatThePausedTurnsText(t *testing.T) {
	said := "The pod loads env from ConfigMap checkout-api, so I'll check that next."
	paused := taskOf(a2a.TaskStateInputRequired, nil,
		artifactOf("before", a2a.NewTextPart(said)), artifactOf("call", callPart(kagv1, "c1")))
	final := func(beforeText string) *a2a.Task {
		return taskOf(a2a.TaskStateCompleted, nil,
			artifactOf("before", a2a.NewTextPart(beforeText)), artifactOf("call", callPart(kagv1, "c1")),
			artifactOf("result", resultPart(kagv1, "c1", okResp)), artifactOf("answer", a2a.NewTextPart("Cause: a misspelt key.")))
	}
	for name, tc := range map[string]struct {
		events []a2a.Event
		want   []string
	}{
		"the final snapshot repeats it unchanged": {
			events: []a2a.Event{chunk("answer", "Cause: a misspelt key.", false), final(said)},
			want:   []string{"Cause: a misspelt key."},
		},
		"a resubscribe's opening snapshot repeats it": {
			events: []a2a.Event{taskOf(a2a.TaskStateWorking, nil, artifactOf("before", a2a.NewTextPart(said))),
				chunk("answer", "Cause: a misspelt key.", false), final(said)},
			want: []string{"Cause: a misspelt key."},
		},
		"the answer changes it": {
			events: []a2a.Event{chunk("answer", "Cause: a misspelt key.", false), final("Corrected.")},
			want:   []string{"Cause: a misspelt key.", "Corrected."},
		},
		// A live chunk is the answer's own, even one that says what the paused turn said.
		"a live chunk says it again": {
			events: []a2a.Event{chunk("before", said, false), final(said)},
			want:   []string{said, "Cause: a misspelt key."},
		},
		// A snapshot that lags a live change, and still holds the paused turn's text, does not put that text back.
		"a lagging snapshot after a live change": {
			events: []a2a.Event{chunk("before", "Corrected.", false), final(said)},
			want:   []string{"Corrected.", "Cause: a misspelt key."},
		},
		// The answer's message then has no text of its own, and the session says the agent finished without output.
		"the answer adds no text": {
			events: []a2a.Event{taskOf(a2a.TaskStateCompleted, nil,
				artifactOf("before", a2a.NewTextPart(said)), artifactOf("call", callPart(kagv1, "c1")),
				artifactOf("result", resultPart(kagv1, "c1", okResp)))},
			want: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := texts(runAnswer(paused, tc.events)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("texts = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("an answer to a question leaves it out too", func(t *testing.T) {
		var got []Event
		asm := newAssembler(func(e Event) { got = append(got, e) })
		asm.seed(paused, false)
		for _, ev := range []a2a.Event{chunk("answer", "Cause: a misspelt key.", false), final(said)} {
			asm.apply(ev)
		}
		if g, want := texts(got), []string{"Cause: a misspelt key."}; !reflect.DeepEqual(g, want) {
			t.Errorf("texts = %q, want %q", g, want)
		}
	})
	// A send that answers nothing, or whose paused task could not be read, has nothing to leave out.
	t.Run("a send that is not an answer delivers every text", func(t *testing.T) {
		_, got := run([]a2a.Event{chunk("answer", "x", false), final(said)})
		if g, want := texts(got), []string{"x", said, "Cause: a misspelt key."}; !reflect.DeepEqual(g, want) {
			t.Errorf("texts = %q, want %q", g, want)
		}
	})
}
