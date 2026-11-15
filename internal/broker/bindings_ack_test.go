package broker

import (
	"slices"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
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

// A Binding that names a reaction on a connection whose platform has no reactions (an empty chat.Caps.AckReaction) gets none: the
// platform would refuse it on every message, and the Binding's AcknowledgmentShown condition says why instead. The same Binding on a
// platform with reactions keeps its own, and so does one on a connection that no platform runs now, which is not known to have none.
func TestABindingThatNamesAReactionOnAPlatformWithNoneGetsNone(t *testing.T) {
	f := newFixture(t)
	set := twoConnectionsSet()
	for i := range set.Bindings {
		set.Bindings[i].Spec.Chat.Acknowledgment = &v1alpha1.ChatAcknowledgment{Emoji: "eyes"}
	}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	f.snaps.p.Store(snap)
	for name, tc := range map[string]struct {
		platforms chat.PlatformSet[platformCaps]
		want      map[string]string
	}{
		"slack-b has no reactions": {chat.PlatformSet[platformCaps]{connA: capsFake{ack: "hourglass"}, connB: capsFake{}}, map[string]string{"sre/oncall": "eyes", "sre/other-bot": ""}},
		"slack-b is not running":   {chat.PlatformSet[platformCaps]{connA: capsFake{}}, map[string]string{"sre/oncall": "", "sre/other-bot": "eyes"}},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := NewBindings(f.snaps, f.reader, f.audit, Options{OnRejected: func(string) {}, Platforms: tc.platforms})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(b.Close)
			for binding, want := range tc.want {
				be, err := b.Backend(binding)
				if err != nil {
					t.Fatalf("Backend(%s) = %v", binding, err)
				}
				if !be.AcknowledgmentEnabled || be.AcknowledgmentEmoji != want {
					t.Errorf("Backend(%s) acknowledgment = enabled %t, %q; want enabled with %q", binding, be.AcknowledgmentEnabled, be.AcknowledgmentEmoji, want)
				}
			}
		})
	}
}

// A Binding's Target says its acknowledgment is not shown when the acknowledgment is enabled, names a reaction, and the platform of its
// connection has no reactions; so the Binding gets an AcknowledgmentShown condition there and nowhere else. A Binding that names none, or
// turns the acknowledgment off, is not told, and neither is one on a platform with reactions (Slack's) or on a connection that no platform
// runs now, which is not known to have none.
func TestATargetSaysWhenItsAcknowledgmentIsNotShown(t *testing.T) {
	off := false
	set := twoConnectionsSet()
	acks := map[string]*v1alpha1.ChatAcknowledgment{
		"sre/oncall": {Emoji: "eyes"}, "ops/oncall": nil, // on slack-a
		"sre/other-bot": {Emoji: "eyes"}, "ops/also-other": {Emoji: "eyes", Enabled: &off}, // on slack-b
	}
	for i := range set.Bindings {
		set.Bindings[i].Spec.Chat.Acknowledgment = acks[set.Bindings[i].Namespace+"/"+set.Bindings[i].Name]
	}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		platforms chat.Resolver[platformCaps]
		unshown   []string
	}{
		"slack-b has no reactions":                       {chat.PlatformSet[platformCaps]{connA: capsFake{ack: "hourglass_flowing_sand"}, connB: capsFake{}}, []string{"sre/other-bot"}},
		"both have reactions":                            {chat.PlatformSet[platformCaps]{connA: capsFake{ack: "hourglass_flowing_sand"}, connB: capsFake{ack: "hourglass_flowing_sand"}}, nil},
		"slack-a has none, and no platform runs slack-b": {chat.PlatformSet[platformCaps]{connA: capsFake{}}, []string{"sre/oncall"}},
		"no platforms were given (nil)":                  {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, target := range bindingTargets(snap, []v1alpha1.ObjectRef{refA, refB}, tc.platforms) {
				if target.AcknowledgmentUnshown {
					got = append(got, target.Binding)
				}
			}
			if !slices.Equal(got, tc.unshown) {
				t.Errorf("Targets whose acknowledgment is not shown = %v; want %v", got, tc.unshown)
			}
		})
	}
}
