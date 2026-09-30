package stubagent_test

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const eightTicks = "tick 1 tick 2 tick 3 tick 4 tick 5 tick 6 tick 7 tick 8 "

func ticks() stubagent.Step {
	return stubagent.Text("tick 1 ", "tick 2 ", "tick 3 ", "tick 4 ", "tick 5 ", "tick 6 ", "tick 7 ", "tick 8 ")
}

func TestOneChunkMatchesSessionFixture(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Text("The answer is 4."))

	evs := c.send(msg("m1", "What is 2+2?", ""))

	rec := fixture(t, "session/01-new-session.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	for i := range evs {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	if got := taskText(last(evs)); got != "The answer is 4." {
		t.Errorf("final task text = %q", got)
	}
}

func TestChunksMatchLiveStreamFixture(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(ticks())

	evs := c.send(msg("m1", "go slowly", ""))

	rec := fixture(t, "livestream/01-slow-stream.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	for i := 3; i < len(evs); i++ {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	if got := assemble(evs); got != eightTicks {
		t.Errorf("append-or-replace rule gives %q, want %q", got, eightTicks)
	}
	_, lastChunk := body(evs[len(evs)-2])
	if lastChunk["append"] != nil || lastChunk["lastChunk"] != true || text(dig(lastChunk, "artifact", "parts")) != eightTicks {
		t.Errorf("lastChunk event = %v, want the full text, lastChunk and no append", lastChunk)
	}
	if got := taskText(last(evs)); got != eightTicks {
		t.Errorf("final task text = %q, want %q", got, eightTicks)
	}
}

func TestTextStepsInATurnShareOneArtifact(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Text("a "), stubagent.Text("b "), stubagent.Text())

	evs := c.send(msg("m1", "hi", ""))

	if got, want := shape(evs[3:]), "artifact, artifact+append, artifact+last, task:COMPLETED"; got != want {
		t.Errorf("shape = %s, want %s", got, want)
	}
	if got := taskText(last(evs)); got != "a b " {
		t.Errorf("final task text = %q, want %q", got, "a b ")
	}
}

func TestTickPausesBetweenChunks(t *testing.T) {
	const tick = 40 * time.Millisecond
	_, agent, c := newAgent(t, stubagent.WithTick(tick))
	agent.Next(stubagent.Text("a", "b", "c"))

	start := time.Now()
	c.send(msg("m1", "hi", ""))

	if took := time.Since(start); took < 2*tick {
		t.Errorf("three chunks took %v, want at least two ticks (%v)", took, 2*tick)
	}
}
