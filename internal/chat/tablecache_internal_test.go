package chat

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

func botBinding(name string) *v1alpha1.Binding {
	b := &v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: name}}
	b.Spec.Chat.Triggers = []v1alpha1.Trigger{{
		Type: v1alpha1.TriggerBotMessage, From: &v1alpha1.BotSource{ID: "B0123456789"},
		Match: map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: "x"},
	}}
	return b
}

// One Binding's compile does not block another's: the cache holds its lock to find an entry and not while it compiles.
func TestACompileDoesNotBlockAnotherBinding(t *testing.T) {
	slow, fast := botBinding("slow"), botBinding("fast")
	release := make(chan struct{})
	started := make(chan struct{})
	c := &tableCache{compile: func(i int, tr *v1alpha1.Trigger) (messageTrigger, error) {
		if tr == &slow.Spec.Chat.Triggers[i] {
			close(started)
			<-release
		}
		return compileTrigger(i, tr)
	}}
	snap, log := &config.Snapshot{}, slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan triggerTable)
	go func() { done <- c.table(snap, slow, log) }()
	<-started // the slow compile holds its entry, and it must not hold the cache

	got := make(chan triggerTable)
	go func() { got <- c.table(snap, fast, log) }()
	select {
	case tab := <-got:
		if len(tab) != 1 {
			t.Fatalf("the fast Binding's table has %d triggers, want 1", len(tab))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a Binding's compile was held up by another's")
	}
	close(release)
	if tab := <-done; len(tab) != 1 {
		t.Fatalf("the slow Binding's table has %d triggers, want 1", len(tab))
	}
}

// Callers that ask for the same Binding's table while it compiles wait for that compile and share its result: a Binding
// is compiled once for a routing table, however many events arrive for it at once.
func TestABindingIsCompiledOncePerRoutingTable(t *testing.T) {
	b := botBinding("alerts")
	var compiles atomic.Int32
	c := &tableCache{compile: func(i int, tr *v1alpha1.Trigger) (messageTrigger, error) {
		compiles.Add(1)
		time.Sleep(20 * time.Millisecond) // long enough for every caller to arrive while it runs
		return compileTrigger(i, tr)
	}}
	snap, log := &config.Snapshot{}, slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if tab := c.table(snap, b, log); len(tab) != 1 {
				t.Errorf("table has %d triggers, want 1", len(tab))
			}
		})
	}
	wg.Wait()
	if n := compiles.Load(); n != 1 {
		t.Fatalf("the trigger was compiled %d times, want 1", n)
	}
	c.table(&config.Snapshot{}, b, log) // a new routing table starts again
	if n := compiles.Load(); n != 2 {
		t.Fatalf("after a reload the trigger was compiled %d times in all, want 2", n)
	}
}

// matched is the head of a field that the regexes read: at most the cap, and never a character cut in two.
func TestMatchedCutsOnACharacterBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"under the cap", strings.Repeat("a", maxMatchedFieldBytes-1), maxMatchedFieldBytes - 1},
		{"at the cap", strings.Repeat("a", maxMatchedFieldBytes), maxMatchedFieldBytes},
		{"ASCII over the cap", strings.Repeat("a", maxMatchedFieldBytes+1), maxMatchedFieldBytes},
		{"a character ending at the cap", strings.Repeat("a", maxMatchedFieldBytes-2) + "é" + "b", maxMatchedFieldBytes},
		{"a character across the cap", strings.Repeat("a", maxMatchedFieldBytes-1) + "é" + "b", maxMatchedFieldBytes - 1},
		{"a four-byte character across the cap", strings.Repeat("a", maxMatchedFieldBytes-2) + "😀" + "b", maxMatchedFieldBytes - 2},
		// Bytes that are no UTF-8 at all have no boundary to back up to; the cut backs up at most one character's worth.
		{"over the cap with no character in it", strings.Repeat("\x80", maxMatchedFieldBytes+10), maxMatchedFieldBytes - (utf8.UTFMax - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := matched(tc.in)
			if len(got) != tc.want || !strings.HasPrefix(tc.in, got) {
				t.Fatalf("matched() is %d bytes, want %d, a prefix of the input", len(got), tc.want)
			}
		})
	}
}
