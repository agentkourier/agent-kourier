package storetest

import (
	"slices"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func (su suite) servedConnections(t *testing.T) {
	served := func(t *testing.T, s store.Store) []string {
		t.Helper()
		got, err := s.ServedConnections(t.Context())
		noErr(t, err, "ServedConnections")
		return got
	}
	replace := func(t *testing.T, s store.Store, connections ...string) {
		t.Helper()
		noErr(t, s.ReplaceServedConnections(t.Context(), connections), "ReplaceServedConnections")
	}

	su.runReopen(t, "no connection is recorded until a set is, and a set survives a restart, listed sorted", func(t *testing.T, s store.Store, reopen func() store.Store) {
		if got := served(t, s); len(got) != 0 {
			t.Fatalf("a new store lists %v; want none", got)
		}
		replace(t, s, "ns/b", "ns/a", "ns-z/a", "Ns/a") // sorted byte for byte: "-" before "/", and upper case before lower
		want := []string{"Ns/a", "ns-z/a", "ns/a", "ns/b"}
		if got := served(t, s); !slices.Equal(got, want) {
			t.Errorf("ServedConnections = %v; want %v", got, want)
		}
		if got := served(t, reopen()); !slices.Equal(got, want) {
			t.Errorf("after a restart ServedConnections = %v; want %v", got, want)
		}
	})

	su.run(t, "ReplaceServedConnections makes the set exactly the one given: the overlap stays, the rest goes and the new is added", func(t *testing.T, s store.Store) {
		replace(t, s, "ns/a", "ns/b", "ns/c")
		replace(t, s, "ns/b", "ns/c", "ns/d")
		if got, want := served(t, s), []string{"ns/b", "ns/c", "ns/d"}; !slices.Equal(got, want) {
			t.Errorf("ServedConnections = %v; want %v", got, want)
		}
		replace(t, s, "ns/b", "ns/c", "ns/d") // the same set again changes nothing
		if got, want := served(t, s), []string{"ns/b", "ns/c", "ns/d"}; !slices.Equal(got, want) {
			t.Errorf("ServedConnections after the same set = %v; want %v", got, want)
		}
		replace(t, s, "ns/x", "ns/x") // a connection listed twice is one
		if got, want := served(t, s), []string{"ns/x"}; !slices.Equal(got, want) {
			t.Errorf("ServedConnections = %v; want %v", got, want)
		}
	})

	su.run(t, "ReplaceServedConnections with an empty set clears the record, and with a nil one too", func(t *testing.T, s store.Store) {
		replace(t, s, "ns/a", "ns/b")
		replace(t, s)
		if got := served(t, s); len(got) != 0 {
			t.Fatalf("ServedConnections after an empty set = %v; want none", got)
		}
		replace(t, s, "ns/a")
		noErr(t, s.ReplaceServedConnections(t.Context(), nil), "ReplaceServedConnections(nil)")
		if got := served(t, s); len(got) != 0 {
			t.Errorf("ServedConnections after a nil set = %v; want none", got)
		}
		replace(t, s) // clearing nothing is not an error
	})

	su.run(t, "ReplaceServedConnections with an empty connection is refused and changes nothing", func(t *testing.T, s store.Store) {
		replace(t, s, "ns/a")
		isErr(t, s.ReplaceServedConnections(t.Context(), []string{"ns/b", ""}), store.ErrInvalid, "an empty connection")
		if got := served(t, s); !slices.Equal(got, []string{"ns/a"}) {
			t.Errorf("ServedConnections = %v; want the set as it was", got)
		}
	})
}
