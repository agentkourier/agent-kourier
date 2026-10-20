package agent

import (
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// chunkOf is a chunk of artifact id: one that adds to the artifact when appendMode is set, else one that replaces it.
func chunkOf(id string, appendMode bool, parts ...*a2a.Part) a2a.Event {
	return &a2a.TaskArtifactUpdateEvent{Append: appendMode, Artifact: &a2a.Artifact{ID: a2a.ArtifactID(id), Parts: parts}}
}

// historyOf is a snapshot of t in state, with the given status message and history.
func historyOf(state a2a.TaskState, status *a2a.Message, history ...*a2a.Message) *a2a.Task {
	t := taskOf(state, status)
	t.History = history
	return t
}

func agentMsg(id string, parts ...*a2a.Part) *a2a.Message {
	return messageOf(a2a.MessageRoleAgent, id, parts...)
}

// Google ADK's default A2A output (adk-go v2.5.0, OutputArtifactPerRun) is one artifact for the whole run: its first
// chunk replaces and every later one appends, so each chunk's parts are numbered from 0 though they follow the parts
// already sent. A part is where it falls in the artifact, and the sealing replace or snapshot that holds them all repeats
// no part.
func TestAppendedChunksOfOneArtifactDeliverEachToolPart(t *testing.T) {
	perRun := func() []a2a.Event {
		return []a2a.Event{
			chunkOf("r", false, callPart(adk, "c1")),
			chunkOf("r", true, resultPart(adk, "c1", okResp)),
			chunkOf("r", true, callPart(adk, "c2")),
			chunkOf("r", true, resultPart(adk, "c2", okResp)),
			chunkOf("r", true, a2a.NewTextPart("done")),
		}
	}
	all := artifactOf("r", callPart(adk, "c1"), resultPart(adk, "c1", okResp), callPart(adk, "c2"), resultPart(adk, "c2", okResp), a2a.NewTextPart("done"))
	live := []string{"call c1", "result c1 " + okJSON, "call c2", "result c2 " + okJSON}
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"every call and result is live, in order", perRun(), live},
		{"then the final task, which holds them all", append(perRun(), taskOf(a2a.TaskStateCompleted, nil, all)), live},
		{"then a sealing replace of the artifact", append(perRun(), chunkOf("r", false, all.Parts...)), live},
		{"a sealing replace puts a part at a new index", []a2a.Event{
			chunkOf("r", false, callPart(adk, "c1")), chunkOf("r", false, a2a.NewTextPart("x"), callPart(adk, "c1"))}, []string{"call c1"}},
		{"parts with no call ID that say the same, in one chunk, are two calls", []a2a.Event{
			chunkOf("r", false, callPart(adk, ""), callPart(adk, "")), chunkOf("r", false, callPart(adk, ""), callPart(adk, ""))}, []string{"call ", "call "}},
		{"parts with no call ID that say the same, appended, are two calls", []a2a.Event{
			chunkOf("r", false, callPart(adk, "")), chunkOf("r", true, callPart(adk, ""))}, []string{"call ", "call "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := run(tc.evs)
			if g := outcomes(got); !reflect.DeepEqual(g, tc.want) {
				t.Errorf("tool events = %q, want %q", g, tc.want)
			}
		})
	}
	t.Run("the text of the appended artifact is said once", func(t *testing.T) {
		_, got := run(append(perRun(), taskOf(a2a.TaskStateCompleted, nil, all)))
		if g := texts(got); !reflect.DeepEqual(g, []string{"done"}) {
			t.Errorf("texts = %q, want done once", g)
		}
	})
}

// A snapshot's history holds the messages a 0.9.x or older-ADK agent sent as its progress, which a stream that was down
// or a task found after a restart never saw. Only the agent's own are read (the history says no state for a message),
// each once.
func TestASnapshotsHistoryIsReadForItsToolParts(t *testing.T) {
	user := messageOf(a2a.MessageRoleUser, "u1", callPart(v09, "cu"))
	m1, m2, m3 := agentMsg("m1", callPart(v09, "c1")), agentMsg("m2", resultPart(v09, "c1", okResp)), agentMsg("m3", a2a.NewTextPart("done"))
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"a snapshot alone, as GetTask returns it", []a2a.Event{historyOf(a2a.TaskStateWorking, m3, user, m1, m2, m3)}, []string{"call c1", "result c1 " + okJSON}},
		{"a task that is over, as Follow finds it after a restart", []a2a.Event{historyOf(a2a.TaskStateCompleted, nil, user, m1, m2, m3)}, []string{"call c1", "result c1 " + okJSON}},
		{"what the stream delivered is not delivered again", []a2a.Event{statusOf(a2a.TaskStateWorking, m1), historyOf(a2a.TaskStateWorking, m3, m1, m2, m3)}, []string{"call c1", "result c1 " + okJSON}},
		{"a second snapshot repeats nothing", []a2a.Event{historyOf(a2a.TaskStateWorking, m3, m1, m2, m3), historyOf(a2a.TaskStateCompleted, nil, m1, m2, m3)}, []string{"call c1", "result c1 " + okJSON}},
		{"the user's messages are not read", []a2a.Event{historyOf(a2a.TaskStateCompleted, nil, user)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
}

// A stream that drops while the agent sends its progress as status messages: what was sent while it was down comes from
// the task's history, then SubscribeToTask opens with the same snapshot and goes on. Each call and result is delivered
// once, in order.
func TestStatusMessageToolPartsSurviveADropAndAResubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m1, m2, m3 := agentMsg("m1", callPart(v09, "c1")), agentMsg("m2", resultPart(v09, "c1", okResp)), agentMsg("m3", a2a.NewTextPart("thinking"))
		m4, m5 := agentMsg("m4", callPart(v09, "c2")), agentMsg("m5", resultPart(v09, "c2", okResp))
		send, sub := make(chan item), make(chan item)
		snap := historyOf(a2a.TaskStateWorking, m3, m1, m2, m3)
		snap.ID, snap.ContextID = "t1", "c1"
		m := newMemConn(t, []chan item{send}, []chan item{sub}, getResult{task: snap})
		s := sendAsync(t, memAgent(t, m, nil))
		s.feed(send, item{ev: working()})
		s.feed(send, item{ev: statusOf(a2a.TaskStateWorking, m1)})
		s.feed(send, item{err: io.ErrUnexpectedEOF}) // m2 and m3 are sent while it is down
		s.feed(sub, item{ev: snap})
		s.feed(sub, item{ev: statusOf(a2a.TaskStateWorking, m4)})
		s.feed(sub, item{ev: statusOf(a2a.TaskStateWorking, m5)})
		s.feed(sub, item{ev: task(a2a.TaskStateCompleted, "done")})
		if err := s.result(); err != nil {
			t.Fatal(err)
		}
		want := []string{"call c1", "result c1 " + okJSON, "call c2", "result c2 " + okJSON}
		if got := outcomes(s.events()); !reflect.DeepEqual(got, want) {
			t.Errorf("tool events = %q, want %q", got, want)
		}
	})
}

// Follow takes a task back after a restart: what the task's history holds of its progress is delivered once.
func TestFollowRecoversTheToolPartsOfAnEarlierRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m1, m2 := agentMsg("m1", callPart(v09, "c1")), agentMsg("m2", resultPart(v09, "c1", okResp))
		snap := historyOf(a2a.TaskStateCompleted, nil, m1, m2)
		snap.ID, snap.ContextID = "t1", "c1"
		h, got := collector()
		if err := memAgent(t, newMemConn(t, nil, nil, getResult{task: snap}), nil).Follow(t.Context(), "t1", h); err != nil {
			t.Fatal(err)
		}
		if want := []string{"call c1", "result c1 " + okJSON}; !reflect.DeepEqual(outcomes(*got), want) {
			t.Errorf("tool events = %q, want %q", outcomes(*got), want)
		}
	})
}

// A result that waits for its call is not lost when the send fails: it is delivered before the send returns, since no
// assembler outlives it to deliver it later.
func TestAHeldResultIsDeliveredWhenTheSendFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := make(chan item)
		m := newMemConn(t, []chan item{send}, nil, getResult{err: errors.New("down")})
		s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 1 }))
		s.feed(send, item{ev: working()})
		s.feed(send, item{ev: toolArtifact("a1", resultPart(kagv1, "c1", okResp))})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		if err := s.result(); !errors.Is(err, ErrStreamLost) {
			t.Fatalf("err = %v, want ErrStreamLost", err)
		}
		if want := []string{"result c1 " + okJSON}; !slices.Equal(outcomes(s.events()), want) {
			t.Errorf("tool events = %q, want %q", outcomes(s.events()), want)
		}
	})
}

// pausedTask is the task of the turn that paused for the approval, as the agent holds it while the approving turn runs:
// the call and the error that stood for the pause (kagent 1.0, approval/01-pause-approve).
func pausedTask(t *testing.T) *a2a.Task {
	t.Helper()
	evs := fixtureEvents(t, "approval/01-pause-approve.response.sse")
	task, ok := evs[len(evs)-1].(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("last event of the pause = %#v, want the INPUT_REQUIRED task", evs[len(evs)-1])
	}
	return task
}

// kinds lists what each event is, as "call", "result", "text", "state" or "final".
func kinds(evs []Event) []string {
	var out []string
	for _, e := range evs {
		switch e.(type) {
		case ToolCallEvent:
			out = append(out, "call")
		case ToolResultEvent:
			out = append(out, "result")
		case TextEvent:
			out = append(out, "text")
		case StateEvent:
			out = append(out, "state")
		case FinalEvent:
			out = append(out, "final")
		}
	}
	return out
}

// runAnswer feeds events to an assembler that answers a task paused as paused says, and returns what it emitted.
func runAnswer(paused *a2a.Task, evs []a2a.Event) (got []Event) {
	asm := newAssembler(func(e Event) { got = append(got, e) })
	asm.seed(paused)
	for _, ev := range evs {
		asm.apply(ev)
	}
	return got
}

// A result for a call that an earlier turn sent (the turn that approves a call streams only the result) is delivered at
// once with its call, found in the task, so the step begins before the text that follows and not after it, where the
// final task's replay of the call would put it. Only the call is taken from the task: its stale error is not delivered.
func TestAResultWhoseCallTheEarlierTurnSentIsDeliveredWithItsCall(t *testing.T) {
	paused := pausedTask(t)
	approve := fixtureEvents(t, "approval/02-approve.response.sse")
	want := []string{"call call_1", `result call_1 {"output":"8"}`}
	t.Run("the call is delivered before the text", func(t *testing.T) {
		got := runAnswer(paused, approve)
		if g := outcomes(got); !reflect.DeepEqual(g, want) {
			t.Errorf("tool events = %q, want %q", g, want)
		}
		k := kinds(got)
		if i, j := slices.Index(k, "result"), slices.Index(k, "text"); j < 0 || i > j {
			t.Errorf("events = %q, want the call and result before the text", k)
		}
	})
	t.Run("a call the send has seen is not taken from the task", func(t *testing.T) {
		seen := []a2a.Event{toolArtifact("a1", callPart(kagv1, "c1")), toolArtifact("a2", resultPart(kagv1, "c1", okResp))}
		got := runAnswer(taskOf(a2a.TaskStateInputRequired, nil, artifactOf("p", callPart(kagv1, "c1"))), seen)
		if g := outcomes(got); !reflect.DeepEqual(g, []string{"call c1", "result c1 " + okJSON}) {
			t.Errorf("tool events = %q, want the call and the result, once", g)
		}
	})
	t.Run("a snapshot is not looked through for the calls of its own results", func(t *testing.T) {
		snap := taskOf(a2a.TaskStateCompleted, nil, artifactOf("a1", resultPart(kagv1, "c1", okResp)), artifactOf("a2", callPart(kagv1, "c1")))
		got := runAnswer(paused, []a2a.Event{snap})
		if g := outcomes(got); !reflect.DeepEqual(g, []string{"call c1", "result c1 " + okJSON}) {
			t.Errorf("tool events = %q, want the call and the result", g)
		}
	})
	t.Run("only the call the result answers is taken from the task", func(t *testing.T) {
		two := taskOf(a2a.TaskStateInputRequired, nil, artifactOf("x", callPart(kagv1, "other")), artifactOf("y", callPart(kagv1, "c1")))
		got := runAnswer(two, []a2a.Event{toolArtifact("a1", resultPart(kagv1, "c1", okResp))})
		if g, want := outcomes(got), []string{"call c1", "result c1 " + okJSON}; !reflect.DeepEqual(g, want) {
			t.Errorf("tool events = %q, want %q: the other call of the task is the paused turn's", g, want)
		}
	})
	// Without the call anywhere the result waits, as it does when the send is not an answer: it comes at the end.
	for name, paused := range map[string]*a2a.Task{
		"the task cannot be read":   nil,
		"the task does not hold it": taskOf(a2a.TaskStateInputRequired, nil, artifactOf("x", callPart(kagv1, "other"))),
	} {
		t.Run("the result waits when "+name, func(t *testing.T) {
			evs := []a2a.Event{toolArtifact("a1", resultPart(kagv1, "call_1", okResp)), chunk("t", "text", false), taskOf(a2a.TaskStateCompleted, nil)}
			asm := newAssembler(nil)
			var got []Event
			asm.emit = func(e Event) { got = append(got, e) }
			if paused != nil {
				asm.seed(paused)
			}
			for _, ev := range evs {
				asm.apply(ev)
			}
			k := kinds(got)
			if text, result := slices.Index(k, "text"), slices.Index(k, "result"); result < text || result != len(k)-2 {
				t.Errorf("events = %q, want the result after the text and just before the end", k)
			}
		})
	}
}

// Answer reads the paused task before it sends, once, so that the answer's own steps are told from the paused turn's.
func TestAnswerReadsThePausedTaskBeforeItSends(t *testing.T) {
	paused := pausedTask(t)
	events := fixtureEvents(t, "approval/02-approve.response.sse")
	ch := make(chan item, len(events))
	for _, ev := range events {
		ch <- item{ev: ev}
	}
	m := newMemConn(t, []chan item{ch}, nil, getResult{task: paused})
	h, got := collector()
	msg := &a2a.Message{ID: "a1", Role: a2a.MessageRoleUser, TaskID: paused.ID, ContextID: paused.ContextID, Parts: a2a.ContentParts{a2a.NewTextPart("yes")}}
	if err := memAgent(t, m, nil).Answer(t.Context(), msg, h); err != nil {
		t.Fatal(err)
	}
	if g, want := m.calls(), []string{"get " + string(paused.ID), "send", "send ended"}; !slices.Equal(g, want) {
		t.Errorf("calls = %q, want %q: the task is read once, before the answer is sent", g, want)
	}
	if g, want := outcomes(*got), []string{"call call_1", `result call_1 {"output":"8"}`}; !reflect.DeepEqual(g, want) {
		t.Errorf("tool events = %q, want %q", g, want)
	}
	if k := kinds(*got); slices.Index(k, "text") < slices.Index(k, "result") {
		t.Errorf("events = %q, want the call and result before the text", k)
	}
}

// An answer whose paused task cannot be read is still sent, and what it can show it shows.
func TestAnswerIsSentWhenThePausedTaskCannotBeRead(t *testing.T) {
	events := fixtureEvents(t, "approval/02-approve.response.sse")
	ch := make(chan item, len(events))
	for _, ev := range events {
		ch <- item{ev: ev}
	}
	m := newMemConn(t, []chan item{ch}, nil, getResult{err: errors.New("down")})
	h, got := collector()
	msg := &a2a.Message{ID: "a1", Role: a2a.MessageRoleUser, TaskID: "01a0f05b-8cc4-772c-ab9c-2506b13dbd40", Parts: a2a.ContentParts{a2a.NewTextPart("yes")}}
	if err := memAgent(t, m, nil).Answer(t.Context(), msg, h); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if _, ok := (*got)[len(*got)-1].(FinalEvent); !ok {
		t.Errorf("last event = %#v, want the final event", (*got)[len(*got)-1])
	}
}

// The task of a turn that went on from an earlier one holds that turn's tool parts too, and they are not this turn's
// news: an answer shows its own steps, and the step of the call that was approved, and not the calls the paused turn made
// before it paused. Two agents that pause after a tool call of their own:
func TestAnAnswerDoesNotShowTheStepsOfTheTurnThatPaused(t *testing.T) {
	// A status-message agent (kagent 0.9.x, older ADK): c0 in the history, then a pause that asks in text.
	question := agentMsg("q", a2a.NewTextPart("Which namespace?"))
	paused := historyOf(a2a.TaskStateInputRequired, question,
		messageOf(a2a.MessageRoleUser, "u1", a2a.NewTextPart("check")), agentMsg("m0", callPart(v09, "c0")), agentMsg("m1", resultPart(v09, "c0", okResp)))
	paused.ID = "t1"
	answered := historyOf(a2a.TaskStateCompleted, nil,
		messageOf(a2a.MessageRoleUser, "u1", a2a.NewTextPart("check")), agentMsg("m0", callPart(v09, "c0")), agentMsg("m1", resultPart(v09, "c0", okResp)),
		question, messageOf(a2a.MessageRoleUser, "u2", a2a.NewTextPart("prod")), agentMsg("m2", callPart(v09, "c1")), agentMsg("m3", resultPart(v09, "c1", okResp)))
	answered.ID = "t1"
	answered.Artifacts = []*a2a.Artifact{artifactOf("a", a2a.NewTextPart("done"))}
	t.Run("a status-message agent, answered", func(t *testing.T) {
		got := runAnswer(paused, []a2a.Event{answered})
		if g, want := outcomes(got), []string{"call c1", "result c1 " + okJSON}; !reflect.DeepEqual(g, want) {
			t.Errorf("tool events = %q, want %q: c0 is the paused turn's", g, want)
		}
	})
	t.Run("a status-message agent that does not echo the answer into the history", func(t *testing.T) {
		noEcho := historyOf(a2a.TaskStateCompleted, nil, messageOf(a2a.MessageRoleUser, "u1", a2a.NewTextPart("check")),
			agentMsg("m0", callPart(v09, "c0")), agentMsg("m1", resultPart(v09, "c0", okResp)), question, agentMsg("m2", callPart(v09, "c1")), agentMsg("m3", resultPart(v09, "c1", okResp)))
		noEcho.ID = "t1"
		got := runAnswer(paused, []a2a.Event{noEcho})
		if g, want := outcomes(got), []string{"call c1", "result c1 " + okJSON}; !reflect.DeepEqual(g, want) {
			t.Errorf("tool events = %q, want %q: c0 is the paused turn's, and the history has no message of the user to say so", g, want)
		}
	})
	t.Run("a status-message agent, followed after a restart", func(t *testing.T) {
		_, got := run([]a2a.Event{answered})
		if g, want := outcomes(got), []string{"call c1", "result c1 " + okJSON}; !reflect.DeepEqual(g, want) {
			t.Errorf("tool events = %q, want %q: c0 is an earlier turn's", g, want)
		}
	})
	// kagent 1.0: x ran and y paused for approval. The answer approves y, streams its result, and the final task replays both.
	t.Run("an approval after an earlier tool of the paused turn", func(t *testing.T) {
		pausedArts := []*a2a.Artifact{artifactOf("x1", callPart(kagv1, "x")), artifactOf("x2", resultPart(kagv1, "x", okResp)),
			artifactOf("y1", callPart(kagv1, "y")), artifactOf("y2", resultPart(kagv1, "y", errResp))}
		final := taskOf(a2a.TaskStateCompleted, nil, append(slices.Clone(pausedArts), artifactOf("y3", resultPart(kagv1, "y", okResp)), artifactOf("t", a2a.NewTextPart("done")))...)
		evs := []a2a.Event{toolArtifact("y3", resultPart(kagv1, "y", okResp)), chunk("t", "done", false), final}
		got := runAnswer(taskOf(a2a.TaskStateInputRequired, nil, pausedArts...), evs)
		if g, want := outcomes(got), []string{"call y", "result y " + okJSON}; !reflect.DeepEqual(g, want) {
			t.Errorf("tool events = %q, want %q: x is the paused turn's", g, want)
		}
	})
}
