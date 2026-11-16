package dialect

import "testing"

func TestInteractionIDIsDeterministic(t *testing.T) {
	ask := Interaction{Kind: AskUser, TaskID: "task-1", RequestID: "req-1"}
	tools := Interaction{Kind: ToolApproval, TaskID: "task-1", Tools: []Tool{{ID: "t1"}, {ID: "t2"}}}
	reordered := tools
	reordered.Tools = []Tool{{ID: "t2"}, {ID: "t1"}}
	id := InteractionID(ask)
	if id != InteractionID(ask) || InteractionID(tools) != InteractionID(reordered) {
		t.Error("the same pause got two IDs")
	}
	if InteractionID(Interaction{TaskID: "ab", RequestID: "c"}) == InteractionID(Interaction{TaskID: "a", RequestID: "bc"}) {
		t.Error("fields that run together hash alike: the length prefixes are not doing their job")
	}
	for _, other := range []Interaction{{Kind: AskUser, TaskID: "task-1", RequestID: "req-2"}, {Kind: AskUser, TaskID: "task-2", RequestID: "req-1"}, tools} {
		if got := InteractionID(other); got == id || len(got) > 40 {
			t.Errorf("%+v: ID %q; want one that is different and short", other, got)
		}
	}
}

// TestInteractionIDIsStored pins the ID of one pause: interactions are stored under it, and a pause that comes again after an
// upgrade must find its row, so the hash never changes.
func TestInteractionIDIsStored(t *testing.T) {
	in := Interaction{Kind: ToolApproval, TaskID: "task-1", RequestID: "req-1", Tools: []Tool{{ID: "t2"}, {ID: "t1"}}}
	if got, want := InteractionID(in), "ix-6035be86e54246ef2e3e980d7a685464"; got != want {
		t.Errorf("InteractionID = %q; want %q", got, want)
	}
}

// TestAnsweredInteraction: the message that answers an interaction names it, and no other message names one.
func TestAnsweredInteraction(t *testing.T) {
	id := InteractionID(Interaction{Kind: AskUser, TaskID: "task-1", RequestID: "req-1"})
	if got := AnswerID(id); got != id+"/answer" {
		t.Errorf("AnswerID(%q) = %q; want the interaction's ID and /answer", id, got)
	}
	for msgID, want := range map[string]string{
		AnswerID(id):        id,
		"reply-1":           "",
		"":                  "",
		"/answer":           "",
		id:                  "",
		id + "/answer/more": "",
	} {
		if got := AnsweredInteraction(msgID); got != want {
			t.Errorf("AnsweredInteraction(%q) = %q; want %q", msgID, got, want)
		}
	}
}
