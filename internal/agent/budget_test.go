package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// namelessText is a chunk of an artifact's text on a stream that has named no task.
func namelessText(text string) a2a.Event {
	return &a2a.TaskArtifactUpdateEvent{Append: true, Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(text)}}}
}

// firstChunk is what the first chunk of text of artifact "a" costs the assembler besides its text: the key of the
// artifact and its entry.
const firstChunk = len("a") + entryBytes

// What the assembler holds of the text is bounded at DefaultMaxReplyBytes, the text of the artifact, its key and its entry
// counted: the largest chunk that fits is kept, and one byte more ends the send.
func TestAssemblerKeepsTextUpToTheBudgetAndNoMore(t *testing.T) {
	fits := DefaultMaxReplyBytes - firstChunk
	for _, tc := range []struct {
		name     string
		size     int
		wantOver bool
	}{
		{"exactly the budget", fits, false},
		{"a byte over the budget", fits + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, got := collector()
			asm := newAssembler(h)
			asm.apply(working())
			asm.apply(delta(strings.Repeat("x", tc.size)))
			if over := asm.over != nil; over != tc.wantOver {
				t.Fatalf("over = %v (%v), want %v", over, asm.over, tc.wantOver)
			}
			if tc.wantOver {
				if !errors.Is(asm.over, dialect.ErrResponseTooLarge) {
					t.Errorf("over = %v, want an error that wraps dialect.ErrResponseTooLarge", asm.over)
				}
				if all := texts(*got); len(all) != 0 {
					t.Errorf("a reply past the budget was delivered: %d texts", len(all))
				}
				return
			}
			if all := texts(*got); len(all) != 1 || len(all[0]) != tc.size {
				t.Errorf("texts = %d, want the text of %d bytes, once", len(all), tc.size)
			}
		})
	}
}

// A text that is replaced and not added to does not grow what the assembler holds: a stream can send a long one in
// pieces that replace each other for ever.
func TestReplacingTextDoesNotGrowTheBudget(t *testing.T) {
	asm := newAssembler(nil)
	asm.limit = 1000 + firstChunk
	asm.apply(working())
	for i := range 5000 {
		asm.apply(task(a2a.TaskStateWorking, strings.Repeat(string(rune('a'+i%26)), 1000)))
	}
	if asm.over != nil {
		t.Fatalf("over after 5000 replacements: %v", asm.over)
	}
}

// What the assembler keeps to tell a repeat is charged too: an agent that sends artifacts, or tool calls, that differ
// by nothing but their names would otherwise grow the maps without a limit, in as few bytes as the wire gives them. Each
// step adds the entries named, and each costs at least entryBytes, so a map that is not charged shows as a shortfall.
func TestAssemblerChargesWhatItKeepsToTellARepeat(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries int // the entries a step adds: the keys of the maps it touches
		step    func(i int) a2a.Event
	}{
		{"artifacts", 1, func(i int) a2a.Event {
			return &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Artifact: &a2a.Artifact{ID: a2a.ArtifactID(fmt.Sprint("a", i)), Parts: a2a.ContentParts{a2a.NewTextPart("x")}}}
		}},
		{"tool calls in messages: placed, said, called", 3, func(i int) a2a.Event {
			id := fmt.Sprint("c", i)
			return statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m"+id, callPart(kagv1, id)))
		}},
		{"tool calls in artifacts: sent, said, called", 3, func(i int) a2a.Event {
			id := fmt.Sprint("c", i)
			return &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Append: true, Artifact: artifactOf("a", callPart(kagv1, id))}
		}},
		{"results that wait for a call: placed, said, live, held", 4, func(i int) a2a.Event {
			id := fmt.Sprint("c", i)
			return statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m"+id, resultPart(kagv1, id, okResp)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const steps = 10
			asm := newAssembler(nil)
			asm.apply(working())
			before := asm.kept
			for i := range steps {
				asm.apply(tc.step(i))
			}
			if got, least := asm.kept-before, steps*tc.entries*entryBytes; got < least {
				t.Errorf("%d steps were charged %d bytes, want at least %d: an entry of a map is not charged", steps, got, least)
			}
			asm.limit = 64 << 10
			for i := steps; i < 10000 && asm.over == nil; i++ {
				asm.apply(tc.step(i))
			}
			if asm.over == nil {
				t.Errorf("no end after 10000 events: %d bytes of 64 KiB held", asm.kept)
			}
		})
	}
}

// A snapshot that is past the budget is the end at once: the states and the final event it would have told are not told.
func TestSnapshotPastTheBudgetTellsNothingMore(t *testing.T) {
	h, got := collector()
	asm := newAssembler(h)
	asm.limit = 100 + firstChunk
	asm.apply(task(a2a.TaskStateCompleted, strings.Repeat("x", 101)))
	if asm.over == nil || len(*got) != 0 || asm.final != nil {
		t.Errorf("over = %v, events = %#v, final = %v; want the end and nothing told", asm.over, *got, asm.final)
	}
}

// A stream that ends clean after the status that settled the task, and a task that read back is past the budget, ends with the error and not as a lost stream.
func TestSettledStreamWhoseTaskIsPastTheBudgetEndsWithTheError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan item)
		m := newMemConn(t, []chan item{ch}, nil, getResult{task: task(a2a.TaskStateCompleted, strings.Repeat("x", 301))})
		s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReplyBytes = 300 + firstChunk }))
		s.feed(ch, item{ev: &a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
		close(ch)
		err := s.result()
		if !errors.Is(err, dialect.ErrResponseTooLarge) || errors.Is(err, ErrStreamLost) {
			t.Errorf("err = %v, want ErrResponseTooLarge and no lost stream", err)
		}
		if got, want := m.calls(), []string{"send", "send ended", "get t1"}; !slices.Equal(got, want) {
			t.Errorf("calls = %q, want %q", got, want)
		}
	})
}

// A result that waited for its call, and has been delivered, is given back.
func TestAResultDeliveredAfterItsCallIsGivenBack(t *testing.T) {
	asm := newAssembler(nil)
	asm.apply(working())
	asm.apply(statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m1", resultPart(kagv1, "c1", okResp))))
	held := asm.kept
	if len(asm.held) != 1 {
		t.Fatalf("held = %d results, want the one that waits", len(asm.held))
	}
	asm.apply(statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m2", callPart(kagv1, "c1"))))
	if len(asm.held) != 0 || asm.kept >= held+len("c1")+3*entryBytes {
		t.Errorf("held = %d, kept = %d (was %d with the result waiting): want the result delivered and its bytes given back", len(asm.held), asm.kept, held)
	}
}

// An assembler that is over keeps and says nothing more, so no event that follows the error is delivered, not even the end.
func TestAssemblerOverSaysNothingMore(t *testing.T) {
	h, got := collector()
	asm := newAssembler(h)
	asm.limit = 100 + firstChunk
	asm.apply(working())
	asm.apply(delta(strings.Repeat("x", 100)))
	asm.apply(delta("x"))
	if asm.over == nil {
		t.Fatal("not over")
	}
	before, over := len(*got), asm.over
	asm.apply(delta("y"))
	asm.apply(task(a2a.TaskStateCompleted, "done"))
	if len(*got) != before || asm.final != nil || !errors.Is(asm.over, over) {
		t.Errorf("after the end: %d events (was %d), final = %v, over = %v; want nothing more", len(*got), before, asm.final, asm.over)
	}
}

// A send whose reply spends the budget fails with the error, at once and once: the stream is closed, and the agent is
// not asked for the task again, since it would send the same. Before the agent has named a task the outcome is
// unknown, as for any other failure there.
func TestSendThatSpendsTheBudgetIsNotReconnected(t *testing.T) {
	tune := func(c *Config) { c.MaxReplyBytes = 300 + firstChunk }
	for _, tc := range []struct {
		name        string
		first, over a2a.Event
		unknown     bool
	}{
		{"after the task is named", working(), delta(strings.Repeat("x", 301)), false},
		{"before any task is named", namelessText(strings.Repeat("y", 100)), namelessText(strings.Repeat("y", 201)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ch := make(chan item)
				m := newMemConn(t, []chan item{ch}, nil)
				s := sendAsync(t, memAgent(t, m, tune))
				s.feed(ch, item{ev: tc.first})
				s.feed(ch, item{ev: tc.over})
				err := s.result()
				if !errors.Is(err, dialect.ErrResponseTooLarge) || errors.Is(err, ErrStreamLost) || errors.Is(err, ErrOutcomeUnknown) != tc.unknown {
					t.Errorf("err = %v; want ErrResponseTooLarge, no lost stream, and ErrOutcomeUnknown = %v", err, tc.unknown)
				}
				if got, want := m.calls(), []string{"send", "send ended"}; !slices.Equal(got, want) {
					t.Errorf("calls = %q, want %q: the stream was closed and nothing asked again", got, want)
				}
				for _, e := range *s.got {
					if _, ok := e.(FinalEvent); ok {
						t.Errorf("events hold a FinalEvent after the error: %#v", *s.got)
					}
				}
			})
		})
	}
}

// A follow is no different: the snapshot it opens with, or the stream after it, past the budget ends it with the error and
// no lost stream, which the session would follow again, to the same end.
func TestFollowThatSpendsTheBudgetIsNotReconnected(t *testing.T) {
	tune := func(c *Config) { c.MaxReplyBytes = 300 + firstChunk }
	big := strings.Repeat("x", 301)
	for _, tc := range []struct {
		name  string
		subs  bool // the snapshot is small, and the stream is what is too large
		calls []string
	}{
		{"a snapshot past the budget", false, []string{"get t1"}},
		{"a finished task past the budget", false, []string{"get t1"}},
		{"a stream past the budget", true, []string{"get t1", "subscribe t1", "subscribe ended"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var m *memConn
				var ch chan item
				if tc.subs {
					ch = make(chan item)
					m = newMemConn(t, nil, []chan item{ch}, getResult{task: task(a2a.TaskStateWorking, "")})
				} else {
					state := a2a.TaskStateWorking
					if strings.HasPrefix(tc.name, "a finished") {
						state = a2a.TaskStateCompleted
					}
					m = newMemConn(t, nil, nil, getResult{task: task(state, big)})
				}
				f := followAsync(t, memAgent(t, m, tune))
				if tc.subs {
					feed(t, ch, item{ev: delta(big)}, f.done)
				}
				err := f.result()
				if !errors.Is(err, dialect.ErrResponseTooLarge) || errors.Is(err, ErrStreamLost) {
					t.Errorf("err = %v; want ErrResponseTooLarge and no lost stream", err)
				}
				if got := m.calls(); !slices.Equal(got, tc.calls) {
					t.Errorf("calls = %q, want %q", got, tc.calls)
				}
			})
		})
	}
}

// pausedWithToolSteps is a task paused for input that holds pairs tool calls and results, in its artifact and again in its
// history as kagent 1.0 keeps them, and the same task as it goes on, completed, with text of its own.
func pausedWithToolSteps(pairs int) (paused, answered *a2a.Task) {
	var parts, hist []*a2a.Part
	for i := range pairs {
		id := fmt.Sprintf("call-%06d-aaaaaaaaaaaaaaaaaaaaaaaaaaaa", i)
		parts = append(parts, callPart(kagv1, id), resultPart(kagv1, id, okResp))
		hist = append(hist, callPart(kagv1, id), resultPart(kagv1, id, okResp))
	}
	steps := artifactOf("artifact-0000000000000000000000000000", parts...)
	history := []*a2a.Message{messageOf(a2a.MessageRoleAgent, "h1", hist...)}
	paused = &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}, History: history, Artifacts: []*a2a.Artifact{steps}}
	answered = &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, History: history,
		Artifacts: []*a2a.Artifact{steps, artifactOf("answer", a2a.NewTextPart("done"))}}
	return paused, answered
}

// The paused task of an answer is the turn that paused, not this one: what the assembler notes of it to tell a repeat is bounded
// by the answer to the call that read it (dialect.MaxResponseBytes), and is not charged to the budget of this turn's reply. A task
// that paused after some five hundred tool steps (kagent 1.0) would otherwise spend the whole budget at the seed, before the
// answer was sent, and could never be answered. What the answer adds is charged as ever.
func TestSeedOfAnAnswerIsNotChargedToTheBudget(t *testing.T) {
	paused, answered := pausedWithToolSteps(1200)
	h, got := collector()
	asm := newAssembler(h)
	asm.seed(paused, false)
	if asm.over != nil || asm.kept != 0 {
		t.Fatalf("after the seed: over = %v, kept = %d; want the budget untouched", asm.over, asm.kept)
	}
	asm.apply(answered)
	if asm.over != nil || asm.final == nil {
		t.Fatalf("after the final task: over = %v, final = %v; want the answer to end", asm.over, asm.final)
	}
	if g := outcomes(*got); len(g) != 0 {
		t.Errorf("tool events = %d, want none: they are the paused turn's", len(g))
	}
	if all := texts(*got); len(all) != 1 || all[0] != "done" {
		t.Errorf("texts = %q, want the answer's own", all)
	}
	// What the answer itself sends is charged against the whole budget, whatever the seed held.
	asm = newAssembler(nil)
	asm.seed(paused, false)
	asm.apply(working())
	asm.apply(delta(strings.Repeat("x", DefaultMaxReplyBytes)))
	if asm.over == nil {
		t.Errorf("a reply of the whole budget after a seed was not over: the seed is not a credit")
	}
}

// An answer to a task that holds more tool steps than the budget is sent to the agent, and its reply is read (the seed spent the
// budget before, and the answer was never sent, with the task left paused and the turn ending with nothing said).
func TestAnswerToATaskOfManyToolStepsIsSent(t *testing.T) {
	paused, answered := pausedWithToolSteps(1200)
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan item, 1)
		ch <- item{ev: answered}
		m := newMemConn(t, []chan item{ch}, nil, getResult{task: paused})
		h, got := collector()
		msg := &a2a.Message{ID: "ans", TaskID: "t1", ContextID: "c1", Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{a2a.NewTextPart("yes")}}
		if err := memAgent(t, m, nil).Answer(t.Context(), msg, h); err != nil {
			t.Fatalf("Answer: %v", err)
		}
		if want := []string{"get t1", "send", "send ended"}; !slices.Equal(m.calls(), want) {
			t.Errorf("calls = %q, want %q", m.calls(), want)
		}
		if _, ok := (*got)[len(*got)-1].(FinalEvent); !ok {
			t.Errorf("last event = %#v, want the final event", (*got)[len(*got)-1])
		}
	})
}
