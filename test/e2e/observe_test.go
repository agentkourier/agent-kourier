package e2e

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// statusOf is the HTTP status of a *StatusError in err, or 0.
func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

func TestWaitThreadReturnsWhenThePredicateHolds(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	root := f.post(t, slack.MsgOptionText("root", false))
	polls := 0
	// The reply is posted during the first poll, which sees the root alone: the second sees both. No
	// sleep decides the order.
	got, err := f.sl.WaitThread(ctx, 5*time.Second, "C1", root, func(th []fakeslack.MessageView) bool {
		if polls++; polls == 1 {
			f.post(t, slack.MsgOptionText("answer", false), slack.MsgOptionTS(root))
		}
		return len(th) >= 2
	})
	if err != nil || polls != 2 || len(got) != 2 || got[1].Text != "answer" {
		t.Fatalf("WaitThread = %+v, %v after %d polls; want the root and the answer after two", got, err, polls)
	}
}

func TestWaitThreadTimesOutWithTheLastThreadSeen(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	root := f.post(t, slack.MsgOptionText("root", false))
	got, err := f.sl.WaitThread(ctx, 100*time.Millisecond, "C1", root, func([]fakeslack.MessageView) bool { return false })
	if !errors.Is(err, ErrTimeout) || len(got) != 1 || got[0].TS != root {
		t.Fatalf("WaitThread = %+v, %v; want ErrTimeout and the thread as last seen", got, err)
	}
}

func TestWaitThreadKeepsWaitingForAThreadThatIsNotThereYet(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	called := false
	_, err := f.sl.WaitThread(ctx, 100*time.Millisecond, "C1", "1.1", func([]fakeslack.MessageView) bool { called = true; return true })
	if !errors.Is(err, ErrTimeout) || statusOf(err) != http.StatusNotFound || called {
		t.Fatalf("WaitThread of an unknown thread = %v (predicate called: %v); want a timeout wrapping the 404, with no predicate call", err, called)
	}
}

func TestWaitThreadEndsWithItsContext(t *testing.T) {
	f := newFixture(t)
	root := f.post(t, slack.MsgOptionText("root", false))
	ctx, cancel := context.WithCancel(t.Context())
	_, err := f.sl.WaitThread(ctx, time.Minute, "C1", root, func([]fakeslack.MessageView) bool { cancel(); return false })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("WaitThread = %v; want the context's error, not a timeout", err)
	}
}

func TestSlackShowsEphemeralsAndReactions(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	root := f.post(t, slack.MsgOptionText("root", false))
	if _, err := f.bot.PostEphemeral("C1", "U1", slack.MsgOptionText("answer every question", false), slack.MsgOptionTS(root)); err != nil {
		t.Fatal(err)
	}
	eph, err := f.sl.Ephemerals(ctx, "C1")
	if err != nil || len(eph) != 1 || eph[0].Recipient != "U1" || eph[0].Text != "answer every question" || eph[0].ThreadTS != root {
		t.Fatalf("Ephemerals = %+v, %v; want the one for U1, in the thread", eph, err)
	}
	if thread, err := f.sl.Thread(ctx, "C1", root); err != nil || len(thread) != 1 {
		t.Fatalf("Thread = %+v, %v; want the ephemeral kept out of it", thread, err)
	}
	if none, err := f.sl.Reactions(ctx, "C1", root); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("Reactions of a message with none = %#v, %v; want an empty list", none, err)
	}
	if err := f.bot.AddReaction("hourglass", slack.ItemRef{Channel: "C1", Timestamp: root}); err != nil {
		t.Fatal(err)
	}
	want := []fakeslack.Reaction{{Name: "hourglass", Users: []string{"UFAKEBOT"}}}
	if got, err := f.sl.Reactions(ctx, "C1", root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Reactions = %+v, %v; want %+v", got, err, want)
	}
	if _, err := f.sl.Reactions(ctx, "C1", "1.1"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("Reactions of an unknown message = %v; want a 404", err)
	}
}
