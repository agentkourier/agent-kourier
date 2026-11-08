package storetest

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// attribution holds what AttributeConnection does to the rows written before connections were recorded. The suite writes them
// the way a caller that names no connection does (an empty one); the migrations' own tests write them as the old schema did.
func (su suite) attribution(t *testing.T) {
	su.runReopen(t, "AttributeConnection gives the connection to the rows that have none, and only those, and a second call changes nothing", func(t *testing.T, s store.Store, reopen func() store.Store) {
		ctx := t.Context()
		for _, o := range []store.OutboxEntry{
			{ID: "old-1", Connection: ""}, {ID: "old-2", Connection: ""}, {ID: "other", Connection: "ns/other"}, {ID: "own", Connection: "ns/own"},
		} {
			o.EventID, o.Role, o.Channel, o.Thread, o.CreatedAt, o.Payload = o.ID, store.OutboxReply, "C1", "1.1", t0, "{}"
			_, err := s.CreateOutbox(ctx, o)
			noErr(t, err, "CreateOutbox "+o.ID)
		}
		noErr(t, s.RecordMessageRef(ctx, "old-2", "9.9"), "a posted row has a connection to attribute too")
		for _, p := range []store.Press{pressOn("", "ix", "U1", t0), pressOn("", "ix", "U2", t0), pressOn("ns/other", "ix", "U1", t0)} {
			_, err := s.RecordPress(ctx, p, roomy)
			noErr(t, err, "RecordPress")
		}

		got, err := s.AttributeConnection(ctx, "ns/own")
		noErr(t, err, "AttributeConnection")
		if want := (store.AttributedRows{Outbox: 2, Presses: 2}); got != want {
			t.Errorf("AttributeConnection = %+v; want %+v", got, want)
		}
		check := func(s store.Store) {
			t.Helper()
			for id, want := range map[string]string{"old-1": "ns/own", "old-2": "ns/own", "other": "ns/other", "own": "ns/own"} {
				if o, err := s.GetOutbox(ctx, id); err != nil || o.Connection != want {
					t.Errorf("outbox row %s = %+v, %v; want connection %q", id, o, err, want)
				}
			}
			presses, err := s.ListPresses(ctx)
			noErr(t, err, "ListPresses")
			var conns []string
			for _, p := range presses {
				conns = append(conns, p.Connection)
			}
			if len(conns) != 3 || conns[0] != "ns/own" || conns[1] != "ns/own" || conns[2] != "ns/other" {
				t.Errorf("press connections = %v; want [ns/own ns/own ns/other]", conns)
			}
		}
		check(s)
		if again, err := s.AttributeConnection(ctx, "ns/own"); err != nil || again != (store.AttributedRows{}) {
			t.Errorf("AttributeConnection again = %+v, %v; want nothing changed", again, err)
		}
		// Rows an older image wrote after a rollback are attributed by the next call, and the rest stay as they were.
		_, err = s.CreateOutbox(ctx, store.OutboxEntry{ID: "rolled-back", EventID: "e", Role: store.OutboxNotice, Channel: "C1", CreatedAt: t0})
		noErr(t, err, "CreateOutbox after the rollback")
		if again, err := s.AttributeConnection(ctx, "ns/own"); err != nil || again != (store.AttributedRows{Outbox: 1}) {
			t.Errorf("AttributeConnection after a late row = %+v, %v; want that row", again, err)
		}
		check(reopen())
	})

	su.run(t, "AttributeConnection with no connection is refused and changes nothing", func(t *testing.T, s store.Store) {
		_, err := s.CreateOutbox(t.Context(), store.OutboxEntry{ID: "old", EventID: "e", Role: store.OutboxNotice, Channel: "C1", CreatedAt: t0})
		noErr(t, err, "CreateOutbox")
		_, err = s.AttributeConnection(t.Context(), "")
		isErr(t, err, store.ErrInvalid, "AttributeConnection of no connection")
		if o := getOutbox(t, s, "old"); o.Connection != "" {
			t.Errorf("the row has connection %q; want it unchanged", o.Connection)
		}
	})

	su.run(t, "AttributeConnection of an empty store changes nothing", func(t *testing.T, s store.Store) {
		if got, err := s.AttributeConnection(t.Context(), "ns/own"); err != nil || got != (store.AttributedRows{}) || got.Total() != 0 {
			t.Errorf("AttributeConnection = %+v, %v; want nothing", got, err)
		}
	})
}
