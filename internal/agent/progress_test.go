package agent

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

// progress lists the ProgressEvents as their text, "-" for one with none.
func progress(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if p, ok := e.(ProgressEvent); ok {
			if p.Text == "" {
				out = append(out, "-")
			} else {
				out = append(out, p.Text)
			}
		}
	}
	return out
}

func textOf(s string) *a2a.Part { return a2a.NewTextPart(s) }

// thought is a text part that key marks as the model's reasoning.
func thought(key, s string) *a2a.Part {
	p := a2a.NewTextPart(s)
	p.Metadata = map[string]any{key: true}
	return p
}

// A live working update that holds no tool part is the agent's activity, whether or not anything in it is understood, and the
// text of the agent's own message is its progress line (spec D45 (1)). The update that makes the task working is the change of
// state, and progress only if it says something.
func TestWorkingUpdatesAreProgress(t *testing.T) {
	working := func(m *a2a.Message) a2a.Event { return statusOf(a2a.TaskStateWorking, m) }
	agentSays := func(id string, parts ...*a2a.Part) *a2a.Message { return messageOf(a2a.MessageRoleAgent, id, parts...) }
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"the first update, with no message, is the change of state", []a2a.Event{working(nil)}, nil},
		{"the first update, with text that is empty, says nothing", []a2a.Event{working(agentSays("m0", textOf(" \n")))}, nil},
		{"each update after it is one unit", []a2a.Event{working(nil), working(nil), working(nil)}, []string{"-", "-"}},
		{"the first update with text says something", []a2a.Event{working(agentSays("m1", textOf("Looking at the pods")))}, []string{"Looking at the pods"}},
		{"the agent's text", []a2a.Event{working(nil), working(agentSays("m1", textOf("Reading logs")))}, []string{"Reading logs"}},
		{"text parts joined by line breaks", []a2a.Event{working(nil), working(agentSays("m1", textOf("one"), textOf("two")))}, []string{"one\ntwo"}},
		{"a data part that is no tool part is opaque", []a2a.Event{working(nil), working(agentSays("m1", a2a.NewDataPart(map[string]any{"x": 1.0})))}, []string{"-"}},
		{"the model's reasoning is not read", []a2a.Event{working(nil), working(agentSays("m1", thought("adk_thought", "I should check the pods"), textOf("Reading the pods")))}, []string{"Reading the pods"}},
		{"an update of reasoning alone has no text", []a2a.Event{working(nil), working(agentSays("m1", thought("kagent_thought", "Private reasoning")))}, []string{"-"}},
		{"kagent 1.0's reasoning", []a2a.Event{working(nil), working(agentSays("m1", thought("kagent.dev/a2a/thought", "Private reasoning")))}, []string{"-"}},
		{"text beside an opaque data part", []a2a.Event{working(nil), working(agentSays("m1", a2a.NewDataPart(map[string]any{"x": 1.0}), textOf("ok")))}, []string{"ok"}},
		{"the user's message echoed back has no text of the agent's", []a2a.Event{working(nil), working(messageOf(a2a.MessageRoleUser, "u1", textOf("yes")))}, []string{"-"}},
		{"a message with no role has none either", []a2a.Event{working(nil), working(messageOf(a2a.MessageRoleUnspecified, "m1", textOf("hi")))}, []string{"-"}},
		{"an update with a tool call is its tool event", []a2a.Event{working(nil), working(agentSays("m1", callPart(v09, "c1"), textOf("calling")))}, nil},
		{"an update with a tool result is its tool event", []a2a.Event{working(nil), working(agentSays("m1", resultPart(adk, "c1", okResp)))}, nil},
		{"a repeated tool part is still a tool part", []a2a.Event{working(nil), working(agentSays("m1", callPart(adk, "c1"))), working(agentSays("m1", callPart(adk, "c1")))}, nil},
		{"a user's echo with a tool part is progress, its part unread", []a2a.Event{working(nil), working(messageOf(a2a.MessageRoleUser, "u1", callPart(adk, "c1")))}, []string{"-"}},
		{"a pause is no progress", []a2a.Event{working(nil), statusOf(a2a.TaskStateInputRequired, agentSays("m1", textOf("Which one?")))}, nil},
		{"the end is no progress", []a2a.Event{working(nil), statusOf(a2a.TaskStateCompleted, agentSays("m1", textOf("Done")))}, nil},
		{"back to working after a pause is the change of state", []a2a.Event{working(nil), statusOf(a2a.TaskStateInputRequired, nil), working(nil)}, nil},
		{"a snapshot is no progress", []a2a.Event{working(nil), taskOf(a2a.TaskStateWorking, agentSays("m1", textOf("Reading logs")))}, nil},
		{"an update after a snapshot that made the task working", []a2a.Event{taskOf(a2a.TaskStateWorking, nil), working(agentSays("m1", textOf("Reading logs")))}, []string{"Reading logs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(progress(got), tc.want) {
				t.Errorf("progress = %q, want %q", progress(got), tc.want)
			}
		})
	}
}

// A ProgressEvent comes where its update did, among the other events, so that a renderer reads it as the latest news.
func TestProgressComesInOrder(t *testing.T) {
	_, got := run([]a2a.Event{
		statusOf(a2a.TaskStateWorking, nil),
		statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m1", textOf("Reading logs"))),
		toolArtifact("a1", callPart(kagv1, "c1")),
		statusOf(a2a.TaskStateWorking, nil),
	})
	var kinds []string
	for _, e := range got {
		switch e.(type) {
		case StateEvent:
			kinds = append(kinds, "state")
		case ProgressEvent:
			kinds = append(kinds, "progress")
		case ToolCallEvent:
			kinds = append(kinds, "call")
		}
	}
	if want := []string{"state", "progress", "call", "progress"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("events = %q, want %q", kinds, want)
	}
}

// kagent 0.9.x over A2A 0.3 (test/fixtures/kagent/v09-tools): every update after the first holds a tool part but the last,
// which repeats the answer that its artifact holds. That one is progress, and its text the answer (accepted, spec D45 (1)).
func TestKagent09TurnHasOneProgressUpdate(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method == "tasks/get" {
			reply(t, w, "v09-tools/02-gettask-after-tool-turn.response.json")
			return
		}
		sse(t, w, "v09-tools/01-tool-turn.response.sse", -1)
	})
	fa.v03 = true
	h, got := collector()
	if err := fa.agentVia(t, generic.New(0)).Send(t.Context(), message("How many namespaces?"), h); err != nil {
		t.Fatal(err)
	}
	if p := progress(*got); !reflect.DeepEqual(p, []string{"The cluster has 46 namespaces."}) {
		t.Errorf("progress = %q, want the answer's text once", p)
	}
}

// kagent 1.0's approving turn (test/fixtures/kagent/approval/02-approve) echoes the user's answer in a working update: activity,
// with no text, as is the empty update after it.
func TestKagentApprovalEchoIsProgressWithNoText(t *testing.T) {
	_, got := run(fixtureEvents(t, "approval/02-approve.response.sse"))
	if p := progress(got); !reflect.DeepEqual(p, []string{"-", "-"}) {
		t.Errorf("progress = %q, want two with no text", p)
	}
}

// A server may open a resubscribe by repeating the status it last sent. That says nothing new of the agent, so it is no
// ProgressEvent: it does not renew the reconnects, or a send on a stream that always drops would never give up. A snapshot does
// not say what was delivered, so one that holds no status message, as kagent 1.0's GetTask may, changes none of this.
func TestARepeatedStatusOnAResubscribeIsNotProgress(t *testing.T) {
	said := messageOf(a2a.MessageRoleAgent, "m1", textOf("Reading logs"))
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status a2a.TaskStatus
		snap   *a2a.Message
		want   []string
	}{
		{"no message", a2a.TaskStatus{State: a2a.TaskStateWorking}, nil, nil},
		{"text, and a snapshot that holds it", a2a.TaskStatus{State: a2a.TaskStateWorking, Message: said}, said, []string{"Reading logs"}},
		{"text, and a snapshot with no message", a2a.TaskStatus{State: a2a.TaskStateWorking, Message: said}, nil, []string{"Reading logs"}},
		{"a timestamp alone", a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &at}, nil, []string{"-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				send, sub1, sub2 := make(chan item), make(chan item), make(chan item)
				snap := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: tc.snap}}
				replay := &a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1", Status: tc.status}
				m := newMemConn(t, []chan item{send}, []chan item{sub1, sub2}, getResult{task: snap})
				s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 2 }))
				s.feed(send, item{ev: working()})
				s.feed(send, item{ev: replay}) // delivered the first time, as it says something, or says nothing and is one unit
				s.feed(send, item{err: io.ErrUnexpectedEOF})
				for _, sub := range []chan item{sub1, sub2} {
					s.feed(sub, item{ev: snap})
					s.feed(sub, item{ev: replay})
					s.feed(sub, item{err: io.ErrUnexpectedEOF})
				}
				if err := s.result(); !errors.Is(err, ErrStreamLost) {
					t.Fatalf("err = %v, want the stream lost", err)
				}
				if n := countOf(m.calls(), "subscribe t1"); n != 2 {
					t.Errorf("%d subscribes, want 2, one per reconnect", n)
				}
				want := tc.want
				if tc.status.Message == nil && tc.status.Timestamp == nil {
					want = []string{"-"} // the update on the first stream, before the drop, is one unit
				}
				if p := progress(s.events()); !reflect.DeepEqual(p, want) {
					t.Errorf("progress = %q, want %q", p, want)
				}
			})
		})
	}
}

// What opens a resubscribed stream and changes nothing, such as the chunk of an artifact the assembler holds, leaves the stream
// still to say anything: a repeated status after it is a repeat.
func TestAChunkThatChangesNothingLeavesTheStreamOpening(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, sub1, sub2 := make(chan item), make(chan item), make(chan item)
		chunk := &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}}
		m := newMemConn(t, []chan item{send}, []chan item{sub1, sub2}, getResult{task: task(a2a.TaskStateWorking, "x")})
		s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 2 }))
		s.feed(send, item{ev: working()})
		s.feed(send, item{ev: chunk})
		s.feed(send, item{ev: working()})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		for _, sub := range []chan item{sub1, sub2} {
			s.feed(sub, item{ev: chunk})
			s.feed(sub, item{ev: working()})
			s.feed(sub, item{err: io.ErrUnexpectedEOF})
		}
		if err := s.result(); !errors.Is(err, ErrStreamLost) {
			t.Fatalf("err = %v, want the stream lost", err)
		}
		if p := progress(s.events()); !reflect.DeepEqual(p, []string{"-"}) {
			t.Errorf("progress = %q, want the one update before the drop", p)
		}
	})
}

// A line the agent sent during a drop reaches the renderer: the snapshot holds it, and the status that opens the stream is the
// first anyone was told of it. It is delivered once, however often the server repeats it.
func TestALineMissedInADropIsDelivered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, sub1, sub2 := make(chan item), make(chan item), make(chan item)
		now := messageOf(a2a.MessageRoleAgent, "m2", textOf("Analysing"))
		snap := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: now}}
		m := newMemConn(t, []chan item{send}, []chan item{sub1, sub2}, getResult{task: snap})
		s := sendAsync(t, memAgent(t, m, nil))
		s.feed(send, item{ev: working()})
		s.feed(send, item{ev: statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m1", textOf("Reading logs")))})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		s.feed(sub1, item{ev: statusOf(a2a.TaskStateWorking, now)})
		s.feed(sub1, item{err: io.ErrUnexpectedEOF})
		s.feed(sub2, item{ev: statusOf(a2a.TaskStateWorking, now)})
		s.feed(sub2, item{ev: task(a2a.TaskStateCompleted, "done")})
		if err := s.result(); err != nil {
			t.Fatal(err)
		}
		if p := progress(s.events()); !reflect.DeepEqual(p, []string{"Reading logs", "Analysing"}) {
			t.Errorf("progress = %q, want each line once", p)
		}
	})
}

// A status that its timestamp, message ID and text identify is a repeat wherever it comes, and any of the three that differs
// makes it a new one. With neither a timestamp nor an ID, only the update that opens a stream can be taken for a repeat.
func TestAStatusIsToldByItsIdentity(t *testing.T) {
	at, later := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 12, 0, 1, 0, time.UTC)
	status := func(ts *time.Time, m *a2a.Message) a2a.Event {
		return &a2a.TaskStatusUpdateEvent{Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: ts, Message: m}}
	}
	agentSays := func(id, text string) *a2a.Message { return messageOf(a2a.MessageRoleAgent, id, textOf(text)) }
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"the same message", []a2a.Event{working(), status(nil, agentSays("m1", "a")), status(nil, agentSays("m1", "a"))}, []string{"a"}},
		{"the same timestamp", []a2a.Event{working(), status(&at, nil), status(&at, nil)}, []string{"-"}},
		{"another timestamp", []a2a.Event{working(), status(&at, nil), status(&later, nil)}, []string{"-", "-"}},
		{"another message", []a2a.Event{working(), status(nil, agentSays("m1", "a")), status(nil, agentSays("m2", "a"))}, []string{"a", "a"}},
		{"the same ID with other text", []a2a.Event{working(), status(nil, agentSays("m1", "a")), status(nil, agentSays("m1", "b"))}, []string{"a", "b"}},
		{"neither, live", []a2a.Event{working(), status(nil, nil), status(nil, nil)}, []string{"-", "-"}},
		{"after a pause, the same status is the change of state", []a2a.Event{working(), status(nil, agentSays("m1", "a")), statusOf(a2a.TaskStateInputRequired, nil), status(nil, agentSays("m1", "a"))}, []string{"a", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(progress(got), tc.want) {
				t.Errorf("progress = %q, want %q", progress(got), tc.want)
			}
		})
	}
}

// A part that the wire holds as null decodes to a nil part. It says nothing, wherever it is, and panics nothing: an agent's bad
// message must not take the broker down.
func TestANullPartSaysNothing(t *testing.T) {
	said := func() a2a.ContentParts { return a2a.ContentParts{nil, a2a.NewTextPart("hi")} }
	agentSays := &a2a.Message{ID: "m1", Role: a2a.MessageRoleAgent, Parts: said()}
	art := &a2a.Artifact{ID: "a", Parts: said()}
	for _, tc := range []struct {
		name string
		ev   a2a.Event
	}{
		{"a working update", &a2a.TaskStatusUpdateEvent{Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: agentSays}}},
		{"a pause", &a2a.TaskStatusUpdateEvent{Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: agentSays}}},
		{"an artifact", &a2a.TaskArtifactUpdateEvent{Artifact: art}},
		{"a snapshot", &a2a.Task{ID: "t", Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: agentSays}, Artifacts: []*a2a.Artifact{art}, History: []*a2a.Message{agentSays}}},
		{"a reply with no task", agentSays},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []Event
			asm := newAssembler(func(e Event) { got = append(got, e) })
			asm.replies = true
			asm.apply(tc.ev)
			var text []string
			for _, e := range got {
				switch e := e.(type) {
				case TextEvent:
					text = append(text, e.Text)
				case ProgressEvent:
					text = append(text, e.Text)
				}
			}
			if tc.name != "a pause" && !slices.Contains(text, "hi") {
				t.Errorf("events = %#v, want the text beside the null part", got)
			}
		})
	}
}

// With nothing to identify it, only the update that opens the stream is taken for a repeat: the next on the stream is the
// agent at work, and renews the reconnects.
func TestAnUpdateAfterTheOpeningOneIsProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, sub1, sub2 := make(chan item), make(chan item), make(chan item)
		m := newMemConn(t, []chan item{send}, []chan item{sub1, sub2}, getResult{task: task(a2a.TaskStateWorking, "")})
		s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 1 }))
		s.feed(send, item{ev: working()})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		s.feed(sub1, item{ev: working()})
		s.feed(sub1, item{ev: working()})
		s.feed(sub1, item{err: io.ErrUnexpectedEOF})
		s.feed(sub2, item{ev: task(a2a.TaskStateCompleted, "done")})
		if err := s.result(); err != nil {
			t.Fatalf("err = %v, want the second stream to finish the task", err)
		}
		if p := progress(s.events()); !reflect.DeepEqual(p, []string{"-"}) {
			t.Errorf("progress = %q, want the one update after the opening one", p)
		}
	})
}

// An update that opens the stream with text not delivered before is news.
func TestNewTextThatOpensAResubscribeIsProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, sub := make(chan item), make(chan item)
		m := newMemConn(t, []chan item{send}, []chan item{sub}, getResult{task: task(a2a.TaskStateWorking, "")})
		s := sendAsync(t, memAgent(t, m, nil))
		s.feed(send, item{ev: working()})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		s.feed(sub, item{ev: statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m1", textOf("Reading logs")))})
		s.feed(sub, item{ev: task(a2a.TaskStateCompleted, "done")})
		if err := s.result(); err != nil {
			t.Fatal(err)
		}
		if p := progress(s.events()); !reflect.DeepEqual(p, []string{"Reading logs"}) {
			t.Errorf("progress = %q, want the new text", p)
		}
	})
}
