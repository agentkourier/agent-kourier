package chat_test

import (
	"errors"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
)

type named string

func TestPlatformSetAnswersEachConnectionWithItsOwn(t *testing.T) {
	set := chat.PlatformSet[named]{"ns/a": "app-a", "ns/b": "app-b"}
	for conn, want := range map[string]named{"ns/a": "app-a", "ns/b": "app-b"} {
		got, err := set.Platform(conn)
		if err != nil || got != want {
			t.Errorf("Platform(%q) = %q, %v; want %q", conn, got, err, want)
		}
	}
}

func TestPlatformSetSaysDownForAConnectionItDoesNotRun(t *testing.T) {
	set := chat.PlatformSet[named]{"ns/a": "app-a"}
	for _, conn := range []string{"ns/other", "", "ns/a "} {
		got, err := set.Platform(conn)
		if !errors.Is(err, chat.ErrConnectionDown) || got != "" {
			t.Errorf("Platform(%q) = %q, %v; want the zero value and ErrConnectionDown", conn, got, err)
		}
	}
	var none chat.PlatformSet[named]
	if _, err := none.Platform("ns/a"); !errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("a nil set: err = %v, want ErrConnectionDown", err)
	}
}

func TestNarrowViewsEachPlatformAsTheNarrowerInterface(t *testing.T) {
	a, b := chattest.NewFake(), chattest.NewFake()
	narrow := chat.Narrow[chat.Permalinker](chat.PlatformSet[chat.Platform]{"ns/a": a, "ns/b": b})
	if p, err := narrow.Platform("ns/a"); err != nil || p != chat.Permalinker(a) {
		t.Errorf("ns/a = %v, %v; want a's own platform", p, err)
	}
	if p, err := narrow.Platform("ns/b"); err != nil || p != chat.Permalinker(b) {
		t.Errorf("ns/b = %v, %v; want b's own platform", p, err)
	}
	if _, err := narrow.Platform("ns/c"); !errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("ns/c = %v; want ErrConnectionDown", err)
	}
	type neverImplemented interface{ NoSuchMethod() }
	if _, err := chat.Narrow[neverImplemented](chat.PlatformSet[chat.Platform]{"ns/a": a}).Platform("ns/a"); err == nil || errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("a platform that is not the narrower interface: err = %v; want an error that is not ErrConnectionDown", err)
	}
}
