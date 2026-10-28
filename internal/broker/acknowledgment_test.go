package broker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/session"
)

// The manager's Unacknowledge queues the removal and then settles its source reply, and settles nothing when the removal
// was not queued: retention must still find that reply and queue it again.
func TestUnacknowledgeSettlesTheSourceOnceItsRemovalIsQueued(t *testing.T) {
	key := session.Key{Connection: "slack", Channel: "C1", Thread: "1.1"}
	n := session.Notice{ID: "reply-1/acknowledgment", Kind: session.NoticeUnacknowledged, MessageRef: "1.1", Reaction: "eyes"}
	errQueue, errSettle := errors.New("outbox unavailable"), errors.New("disk full")
	for _, tc := range []struct {
		name                string
		queueErr, settleErr error
		wantSettled         [][]string
		wantErr             error
		wantStep            string // what the error says failed, so that a log of it does not blame the queue for the settle
	}{
		{name: "queued and settled", wantSettled: [][]string{{"reply-1"}}},
		{name: "not queued", queueErr: errQueue, wantErr: errQueue, wantStep: "queue the acknowledgment removal of reply-1"},
		{name: "queued, not settled", settleErr: errSettle, wantSettled: [][]string{{"reply-1"}}, wantErr: errSettle, wantStep: "settle reply-1, whose acknowledgment removal is queued"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queued []session.Notice
			var settled [][]string
			fn := unacknowledge(func(_ context.Context, k session.Key, got session.Notice) error {
				if k != key {
					t.Errorf("queued in %+v; want %+v", k, key)
				}
				queued = append(queued, got)
				return tc.queueErr
			}, func(_ context.Context, ids []string) error {
				settled = append(settled, slices.Clone(ids))
				return tc.settleErr
			})
			err := fn(t.Context(), key, "reply-1", n)
			if !errors.Is(err, tc.wantErr) || (err != nil && !strings.HasPrefix(err.Error(), tc.wantStep+": ")) {
				t.Errorf("Unacknowledge = %v; want %v, after %q", err, tc.wantErr, tc.wantStep)
			}
			if !slices.Equal(queued, []session.Notice{n}) {
				t.Errorf("queued = %+v; want the notice as it came", queued)
			}
			if !slices.EqualFunc(settled, tc.wantSettled, slices.Equal) {
				t.Errorf("settled = %v; want %v", settled, tc.wantSettled)
			}
		})
	}
}
