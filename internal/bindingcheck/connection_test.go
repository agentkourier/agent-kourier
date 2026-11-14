package bindingcheck

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"testing"
)

// connectionLog is a ChannelChecker and a GroupChecker that records the connection each probe was made for, with what it was asked.
type connectionLog struct {
	mu       sync.Mutex
	channels [][2]string // connection, channel
	groups   [][]string  // connection, then the groups
}

func (l *connectionLog) CheckChannel(_ context.Context, connection, channel string) (Outcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.channels = append(l.channels, [2]string{connection, channel})
	return ok(), nil
}

func (l *connectionLog) CheckGroups(_ context.Context, connection string, groups []string) (Outcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.groups = append(l.groups, append([]string{connection}, groups...))
	return ok(), nil
}

// A probe is made for the chat connection of its Target, so that a checker that holds one bot for each connection reads with the bot of
// the Binding's own: two Bindings that name one channel on two connections are two probes, each with its connection.
func TestChannelAndGroupProbesCarryTheConnectionOfTheirTarget(t *testing.T) {
	g := &targets{}
	g.set("r1",
		Target{Binding: "a/one", Connection: "ns-a/conn", Channel: "C1", ApproverGroups: []string{"S1"}},
		Target{Binding: "b/two", Connection: "ns-b/conn", Channel: "C1", ApproverGroups: []string{"S1", "S2"}},
		Target{Binding: "b/three", Channel: "C2"})
	seen := &connectionLog{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: seen, Groups: seen}, func(*harness) {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		channels := slices.SortedFunc(slices.Values(seen.channels), func(a, b [2]string) int {
			return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
		})
		want := [][2]string{{"", "C2"}, {"ns-a/conn", "C1"}, {"ns-b/conn", "C1"}}
		if !slices.Equal(channels, want) {
			t.Errorf("channel probes = %v; want %v: one for each Target, with its connection (empty when the Target names none)", channels, want)
		}
		if len(seen.groups) != 2 {
			t.Fatalf("group probes = %v; want one for each Target that has groups", seen.groups)
		}
		for _, call := range seen.groups {
			if (call[0] == "ns-a/conn" && !slices.Equal(call[1:], []string{"S1"})) || (call[0] == "ns-b/conn" && !slices.Equal(call[1:], []string{"S1", "S2"})) {
				t.Errorf("group probe %v; want each Target's own groups with its own connection", call)
			}
		}
	})
}
