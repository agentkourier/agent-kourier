package broker

import (
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// TestThreadContextReadsThroughThePlatformOfTheTurnsConnection: the replies of a thread, and the names of their authors, are
// read from the app of the turn's connection, though two apps have threads with the same IDs; a turn on a connection no
// platform runs goes with the fixed line that says the replies could not be read.
func TestThreadContextReadsThroughThePlatformOfTheTurnsConnection(t *testing.T) {
	w := newTCWorld(t)
	fakeB := chattest.NewFake()
	fakeB.AddUser(chat.User{ID: "U1", Name: "Barbara"}, false)
	readerB := &windowReader{FakePlatform: fakeB}
	w.win.window = []chat.HistoryMessage{msg(1, "U1", "said on a")}
	readerB.window = []chat.HistoryMessage{msg(1, "U1", "said on b")}
	tc := mustThreadContext(t, threadContextConfig{
		Platforms: chat.PlatformSet[chat.Platform]{tcConn: w.win, "ns/team": readerB}, Replies: w.turns,
		After: func(time.Duration) <-chan time.Time { c := make(chan time.Time, 1); c <- time.Time{}; return c },
	})
	section := func(conn string) string {
		return tc.Section(w.ctx, session.Key{Connection: conn, Channel: tcChannel, Thread: tcThread}, tcBinding, "100.000100", "100.000900", false)
	}

	if a, b := section(tcConn), section("ns/team"); strings.Join(shownTexts(a), "|") != "said on a" || strings.Join(shownTexts(b), "|") != "said on b" {
		t.Errorf("sections:\na: %s\nb: %s\nwant each connection's own reply", a, b)
	}
	if got := entries(section("ns/team")); len(got) != 1 || got[0].from != "Barbara" {
		t.Errorf("b's reply is from %+v; want the name b's platform gives U1", got)
	}
	if len(w.win.reads()) != 1 || len(readerB.reads()) != 2 {
		t.Errorf("a was read %d times and b %d; want once and twice: each read went to its own connection's platform", len(w.win.reads()), len(readerB.reads()))
	}
	if got := section("ns/down"); got != threadReadFailed {
		t.Errorf("section on a connection that is down = %q; want %q", got, threadReadFailed)
	}
}

// TestUsersAreResolvedOnTheConnectionOfTheTurn: the user of a turn is looked up on the platform of the turn's connection.
func TestUsersAreResolvedOnTheConnectionOfTheTurn(t *testing.T) {
	a, b := chattest.NewFake(), chattest.NewFake()
	a.AddUser(chat.User{ID: "U1", Name: "Ada"}, false)
	b.AddUser(chat.User{ID: "U1", Name: "Barbara"}, false)
	u := users{chat.PlatformSet[chat.Platform]{"ns/a": a, "ns/b": b}}
	for conn, want := range map[string]string{"ns/a": "Ada", "ns/b": "Barbara"} {
		if got, err := u.ResolveUser(t.Context(), conn, "U1"); err != nil || got != (dialect.User{ID: "U1", Name: want}) {
			t.Errorf("ResolveUser on %s = %+v, %v; want %s", conn, got, err, want)
		}
	}
	if _, err := u.ResolveUser(t.Context(), "ns/down", "U1"); err == nil {
		t.Error("ResolveUser on a connection that is down succeeded")
	}
}
