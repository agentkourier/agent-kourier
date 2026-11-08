package broker

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// capsFake is a platform that has only its capabilities, with the given acknowledgment reaction.
type capsFake struct{ ack string }

func (c capsFake) Capabilities() chat.Caps { return chat.Caps{AckReaction: c.ack} }

// A Binding that names no acknowledgment reaction gets the one of its own connection's platform: the two connections of the config run
// platforms with different defaults, and each Binding gets the default of the platform that runs its connection. A connection that no
// platform runs has no default to give, so the Binding shows nothing until it names a reaction, as it does on a platform with none.
func TestABindingGetsTheAcknowledgmentReactionOfItsOwnConnectionsPlatform(t *testing.T) {
	f := newFixture(t)
	f.snaps.p.Store(twoConnections(t))
	platforms := chat.PlatformSet[platformCaps]{connA: capsFake{ack: "hourglass"}, connB: capsFake{ack: "eyes"}}
	b, err := NewBindings(f.snaps, f.reader, f.audit, Options{OnRejected: func(string) {}, Platforms: platforms})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	for binding, want := range map[string]string{"sre/oncall": "hourglass", "ops/oncall": "hourglass", "sre/other-bot": "eyes", "ops/also-other": "eyes"} {
		be, err := b.Backend(binding)
		if err != nil {
			t.Fatalf("Backend(%s) = %v", binding, err)
		}
		if !be.AcknowledgmentEnabled || be.AcknowledgmentEmoji != want {
			t.Errorf("Backend(%s) acknowledgment = enabled %t, %q; want enabled with %q, its connection's default", binding, be.AcknowledgmentEnabled, be.AcknowledgmentEmoji, want)
		}
	}

	// A connection with no platform running: no default.
	down := chat.PlatformSet[platformCaps]{connA: capsFake{ack: "hourglass"}}
	b2, err := NewBindings(f.snaps, f.reader, f.audit, Options{OnRejected: func(string) {}, Platforms: down})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.Close)
	be, err := b2.Backend("sre/other-bot")
	if err != nil {
		t.Fatal(err)
	}
	if be.AcknowledgmentEmoji != "" {
		t.Errorf("acknowledgment emoji of a Binding on a connection that no platform runs = %q; want none", be.AcknowledgmentEmoji)
	}
	// Without any platforms the same: Bindings built for a test or a tool that runs none.
	b3, err := NewBindings(f.snaps, f.reader, f.audit, Options{OnRejected: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b3.Close)
	if be, err := b3.Backend("sre/oncall"); err != nil || be.AcknowledgmentEmoji != "" {
		t.Errorf("Backend with no platforms = %q, %v; want no emoji and no error", be.AcknowledgmentEmoji, err)
	}
}
