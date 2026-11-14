package outbox_test

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// TestRowsCarryTheConnectionOfTheirPostOrTheDefault: a post, a reaction and the removal of one are stored with the connection
// their producer named, and with Config.DefaultConnection when it named none. With no default they have none, as the rows
// written before connections were recorded do.
func TestRowsCarryTheConnectionOfTheirPostOrTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name, def, named, want string
	}{
		{"the producer names one", "ns/own", "ns/team", "ns/team"},
		{"the producer names none", "ns/own", "", "ns/own"},
		{"no default, none named", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := sqlitetest.Open(t)
			o := mustNew(t, outbox.Config{Store: st, Platforms: chattest.Same[outbox.Platform](&poster{}), Now: func() time.Time { return t0 }, DefaultConnection: tc.def,
				Logger: testutil.DiscardLogger()})
			ctx := t.Context()

			post := reply("ev1")
			post.Connection = tc.named
			id, err := o.Enqueue(ctx, post)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			react := queuedReaction
			react.Connection = tc.named
			reactID, err := o.EnqueueReaction(ctx, react)
			if err != nil {
				t.Fatalf("EnqueueReaction: %v", err)
			}
			unreactID, err := o.EnqueueUnreaction(ctx, outbox.Reaction{Source: "other/queued", Channel: "C1", TS: "1.5", Name: "hourglass", Connection: tc.named})
			if err != nil {
				t.Fatalf("EnqueueUnreaction: %v", err)
			}
			for _, rowID := range []string{id, reactID, unreactID} {
				if row, err := st.GetOutbox(ctx, rowID); err != nil || row.Connection != tc.want {
					t.Errorf("row %s = %+v, %v; want connection %q", rowID, row, err, tc.want)
				}
			}
		})
	}
}

// TestSendStoresTheConnectionOfThePost: Send writes the same row Enqueue does.
func TestSendStoresTheConnectionOfThePost(t *testing.T) {
	st := sqlitetest.Open(t)
	o := mustNew(t, outbox.Config{Store: st, Platforms: chattest.Same[outbox.Platform](&poster{}), Now: func() time.Time { return t0 }, DefaultConnection: "ns/own", CallTimeout: time.Second,
		Logger: testutil.DiscardLogger()})
	if _, err := o.Send(t.Context(), reply("ev1")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if row, err := st.GetOutbox(t.Context(), outbox.ID("ev1", store.OutboxReply)); err != nil || row.Connection != "ns/own" {
		t.Errorf("row = %+v, %v; want connection ns/own, the default", row, err)
	}
}
