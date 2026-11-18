package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// The first turn of a session that a person's mention opened inside an existing thread asks with no lower bound and not for the whole
// thread (session.Config.ThreadContext): on a platform that carries a thread's root (chat.Caps.RootContext), the turn carries the
// message that opened the thread, whoever wrote it, and nothing else of the thread (spec 8.2, D55).

// rootFirstTurn asks the thread context of the world for the first turn of a session opened inside the thread, by a mention at
// "100.000900", over the platform p.
func (w *tcWorld) rootFirstTurn(p chat.Platform) string {
	w.t.Helper()
	return w.build(p).Section(w.ctx, tcKey, tcBinding, "", "100.000900", false)
}

// carriesRoots makes the world's fake a platform that carries a thread's root.
func (w *tcWorld) carriesRoots() {
	c := w.fake.Capabilities()
	c.RootContext = true
	w.fake.SetCaps(c)
}

// appID is the sender of another app's posts, as Microsoft Teams names an app: a fake one.
const appID = "00000000-0000-4000-8000-000000000001"

// An alert's card that another app posted at the thread's root goes with a person's first mention in it; the replies after it do not,
// a person's or another app's. The root is read in one call from the thread's start up to the mention, which asks for one reply only.
func TestThreadContextCarriesTheRootThatAnotherAppPosted(t *testing.T) {
	w := newTCWorld(t)
	w.carriesRoots()
	w.win.window = []chat.HistoryMessage{
		msg(3, "U1", "a person's reply before the mention"),
		botPost(4, "", chat.BotMessage{ID: appID, Body: "another app's reply"}, ""),
	}
	r := &rootedReader{windowReader: w.win, root: opener(botPost(1, "", chat.BotMessage{ID: appID, Body: "FIRING HighLatency: p99 is 2s"}, ""))}

	got := w.rootFirstTurn(r)

	if want := []entry{{"", appID, "FIRING HighLatency: p99 is 2s"}}; fmt.Sprint(entries(got)) != fmt.Sprint(want) {
		t.Errorf("entries = %+v; want the root alone, by its app: %+v\n%s", entries(got), want, got)
	}
	if !strings.HasPrefix(got, "The message that opened this thread") {
		t.Errorf("the section is not the root's block:\n%s", got)
	}
	if c := w.win.reads(); len(c) != 1 || c[0].after != "" || c[0].before != "100.000900" || c[0].limit != 1 {
		t.Errorf("reads = %+v; want one from the start up to the mention, for one reply", c)
	}
	if o := w.outcomes(); fmt.Sprint(o) != fmt.Sprint(map[string]float64{tcBinding + " read": 1}) {
		t.Errorf("outcomes = %v; want one read", o)
	}
}

// A root that a person wrote goes too, by their name; one that is not a message the thread shows does not.
func TestThreadContextCarriesTheRootWhoeverWroteIt(t *testing.T) {
	ownRoot := opener(msg(1, "UME", "Agent Kourier's own post"))
	ownRoot.Own = true
	ignored := opener(msg(1, "U1", "a system event"))
	ignored.Ignored = true
	for _, tt := range []struct {
		name string
		root chat.HistoryMessage
		turn bool // the root was itself a turn
		want []entry
	}{
		{name: "a person's", root: opener(msg(1, "U2", "the card service is down")), want: []entry{{"Grace", "U2", "the card service is down"}}},
		{name: "a bot's, with a user", root: opener(botPost(1, "U3", chat.BotMessage{ID: "B0HOOK"}, "a bot's text")), want: []entry{{"Linus", "U3", "a bot's text"}}},
		{name: "Agent Kourier's own", root: ownRoot},
		{name: "one the adapter would drop", root: ignored},
		{name: "blank once normalized", root: opener(msg(1, "U2", "\u200b\u200d"))},
		{name: "a turn already", root: opener(msg(1, "U2", "a mention that was a turn")), turn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newTCWorld(t)
			w.carriesRoots()
			if tt.turn {
				w.turns.ids[idOf(tt.root)] = true
			}
			got := w.rootFirstTurn(&rootedReader{windowReader: w.win, root: tt.root})
			if fmt.Sprint(entries(got)) != fmt.Sprint(tt.want) {
				t.Errorf("entries = %+v; want %+v\n%s", entries(got), tt.want, got)
			}
			outcome := "read"
			if tt.want == nil {
				outcome = "empty"
			}
			if o := w.outcomes(); fmt.Sprint(o) != fmt.Sprint(map[string]float64{tcBinding + " " + outcome: 1}) {
				t.Errorf("outcomes = %v; want one %s", o, outcome)
			}
		})
	}
}

// A post with no user is named by its bot, as in a whole thread's block, and costs no lookup.
func TestThreadContextNamesARootWithNoUserByItsBot(t *testing.T) {
	w := newTCWorld(t)
	w.carriesRoots()
	got := w.rootFirstTurn(&rootedReader{windowReader: w.win, root: opener(botPost(1, "", chat.BotMessage{ID: "B0ALERTS", Title: "FIRING"}, ""))})
	if want := []entry{{"", "B0ALERTS", "FIRING"}}; fmt.Sprint(entries(got)) != fmt.Sprint(want) {
		t.Errorf("entries = %+v; want %+v", entries(got), want)
	}
	if len(w.win.resolved) != 0 {
		t.Errorf("looked up %v; a post with no user needs no name", w.win.resolved)
	}
}

// missingRoot is a platform that reads a thread whose root it no longer has: ThreadFromStart is the window alone.
type missingRoot struct{ *windowReader }

func (m *missingRoot) ThreadFromStart(ctx context.Context, t chat.Target, before string, limit int) ([]chat.HistoryMessage, int, error) {
	return m.ThreadBetween(ctx, t, "", before, limit)
}

// A thread whose root the platform no longer has starts with its first reply: there is no root to carry, and the reply is not one.
func TestThreadContextCarriesNoReplyInPlaceOfAMissingRoot(t *testing.T) {
	w := newTCWorld(t)
	w.carriesRoots()
	w.win.window = []chat.HistoryMessage{msg(3, "U1", "the first reply")}
	if got := w.rootFirstTurn(&missingRoot{w.win}); got != "" {
		t.Errorf("section = %q; want nothing", got)
	}
	if o := w.outcomes(); fmt.Sprint(o) != fmt.Sprint(map[string]float64{tcBinding + " empty": 1}) {
		t.Errorf("outcomes = %v; want one empty", o)
	}
}

// A platform that does not carry a thread's root (Slack) is not read and not counted for a first turn: the turn goes with the person's
// message alone, as it did before roots were carried. Neither is a connection that is not running, whose platform cannot be asked.
func TestThreadContextOfAFirstTurnOnAPlatformWithoutRootsIsNothing(t *testing.T) {
	t.Run("the platform does not carry roots", func(t *testing.T) {
		w := newTCWorld(t)
		r := &rootedReader{windowReader: w.win, root: opener(botPost(1, "", chat.BotMessage{ID: appID, Body: "FIRING"}, ""))}
		if got := w.rootFirstTurn(r); got != "" {
			t.Errorf("section = %q; want nothing", got)
		}
		if c := w.win.reads(); len(c) != 0 {
			t.Errorf("reads = %+v; want none", c)
		}
		if o := w.outcomes(); len(o) != 0 {
			t.Errorf("outcomes = %v; want no count", o)
		}
		if w.logs.Len() != 0 {
			t.Errorf("logs: %s; want none", w.logs)
		}
	})
	t.Run("the connection is not running", func(t *testing.T) {
		w := newTCWorld(t)
		tc := mustThreadContext(t, threadContextConfig{Platforms: chat.PlatformSet[chat.Platform]{}, Replies: w.turns, Logger: slog.New(slog.NewTextHandler(w.logs, nil))})
		if got := tc.Section(w.ctx, tcKey, tcBinding, "", "100.000900", false); got != "" {
			t.Errorf("section = %q; want nothing", got)
		}
		if o := w.outcomes(); len(o) != 0 {
			t.Errorf("outcomes = %v; want no count", o)
		}
	})
}

// A later turn's window opens after the turn before it, so the root, which the first turn carried, is not read or carried again. A
// mention that opens its own thread has no root before it, and reads nothing.
func TestThreadContextCarriesTheRootOnTheFirstTurnAlone(t *testing.T) {
	w := newTCWorld(t)
	w.carriesRoots()
	w.win.window = []chat.HistoryMessage{msg(3, "U1", "go on")}
	r := &rootedReader{windowReader: w.win, root: opener(botPost(1, "", chat.BotMessage{ID: appID, Body: "FIRING"}, ""))}
	tc := w.build(r)
	if got := tc.Section(w.ctx, tcKey, tcBinding, "100.000100", "100.000900", false); strings.Contains(got, "FIRING") || !sameTexts(shownTexts(got), []string{"go on"}) {
		t.Errorf("a later turn: section:\n%s", got)
	}
	reads := len(w.win.reads())
	if got := tc.Section(w.ctx, tcKey, tcBinding, "", tcThread, false); got != "" || len(w.win.reads()) != reads {
		t.Errorf("a mention that opens its thread = %q, %d reads; want nothing and no read", got, len(w.win.reads())-reads)
	}
}

// A thread too long to read past its root still gives the root, and the turn needs nothing else of it: no line. If the root did not
// come back with the error, the turn goes with the line. A read that fails is the line; a platform that cannot read is the message alone.
func TestThreadContextOfARootThatCannotBeRead(t *testing.T) {
	root := opener(botPost(1, "", chat.BotMessage{ID: appID, Body: "FIRING"}, ""))
	t.Run("too long, with the root", func(t *testing.T) {
		w := newTCWorld(t)
		w.carriesRoots()
		got := w.rootFirstTurn(&tooLongReader{windowReader: w.win, root: root})
		if want := []entry{{"", appID, "FIRING"}}; fmt.Sprint(entries(got)) != fmt.Sprint(want) || strings.Contains(got, threadReadFailed) {
			t.Errorf("section:\n%s", got)
		}
		if o := w.outcomes(); fmt.Sprint(o) != fmt.Sprint(map[string]float64{tcBinding + " read": 1}) {
			t.Errorf("outcomes = %v; want one read", o)
		}
	})
	t.Run("too long, without the root", func(t *testing.T) {
		w := newTCWorld(t)
		w.carriesRoots()
		w.win.script = []error{fmt.Errorf("teams thread read: %w: more than 20 pages", chat.ErrThreadTooLong)}
		if got := w.rootFirstTurn(&rootedReader{windowReader: w.win, root: root}); got != threadReadFailed {
			t.Errorf("section = %q; want the line", got)
		}
	})
	t.Run("a read that failed", func(t *testing.T) {
		w := newTCWorld(t)
		w.carriesRoots()
		w.win.script = []error{errors.New("network is down")}
		if got := w.rootFirstTurn(&rootedReader{windowReader: w.win, root: root}); got != threadReadFailed {
			t.Errorf("section = %q; want the line", got)
		}
		if o := w.outcomes(); fmt.Sprint(o) != fmt.Sprint(map[string]float64{tcBinding + " failed": 1}) {
			t.Errorf("outcomes = %v; want one failed", o)
		}
	})
	t.Run("the reply queue cannot say whether it was a turn", func(t *testing.T) {
		w := newTCWorld(t)
		w.carriesRoots()
		w.turns.err = errors.New("database is locked")
		if got := w.rootFirstTurn(&rootedReader{windowReader: w.win, root: root}); got != threadReadFailed {
			t.Errorf("section = %q; want the line", got)
		}
	})
	t.Run("a platform that cannot read a thread", func(t *testing.T) {
		w := newTCWorld(t)
		w.carriesRoots()
		if got := w.rootFirstTurn(noReads{w.fake}); got != "" {
			t.Errorf("section = %q; want nothing", got)
		}
		if o := w.outcomes(); fmt.Sprint(o) != fmt.Sprint(map[string]float64{tcBinding + " unsupported": 1}) {
			t.Errorf("outcomes = %v; want one unsupported", o)
		}
	})
}

// The root's section is render.ThreadRoot's block of it, named.
func TestThreadContextOfTheRootIsTheRendersBlock(t *testing.T) {
	w := newTCWorld(t)
	w.carriesRoots()
	got := w.rootFirstTurn(&rootedReader{windowReader: w.win, root: opener(msg(1, "U1", "what opened it"))})
	if want := render.ThreadRoot(render.ThreadReply{UserID: "U1", Name: "Ada", Text: "what opened it"}); got != want {
		t.Errorf("section:\n%s\nwant:\n%s", got, want)
	}
}
