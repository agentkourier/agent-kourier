package teams

import (
	"context"
	"errors"
	"time"
)

// PollPosition is where the poll of a channel's messages stands (poll.go): the deltaLink of the channel's last delta read whose messages
// were all handed on, and when the poll that made that read began.
type PollPosition struct {
	// Channel is the channel's ID, 19:…@thread.tacv2: the key.
	Channel string
	// DeltaLink is the @odata.deltaLink that Microsoft Graph gave the read. It carries Graph's state token: it is never logged.
	DeltaLink string
	// PolledAt is when the poll that made the read began.
	PolledAt time.Time
}

// PollPositions keeps the PollPosition of each polled channel of a connection, so that a run of the connection resumes the channel's
// delta where the last run left it: a restart or a change of leader then reads what was posted meanwhile. Only the leader polls, so only
// the leader writes. All four are safe for concurrent use.
type PollPositions interface {
	// Load returns the position of channel of the connection conn ("namespace/name"), and false when none is kept.
	Load(ctx context.Context, conn, channel string) (PollPosition, bool, error)
	// Save keeps p as the position of p.Channel of conn, in place of the one kept.
	Save(ctx context.Context, conn string, p PollPosition) error
	// Forget deletes the position of channel of conn; a channel with none is not an error.
	Forget(ctx context.Context, conn, channel string) error
	// ForgetUnbound deletes the position of each channel of conn that is not in bound, the channels that the connection's Bindings
	// name; an empty bound deletes them all.
	ForgetUnbound(ctx context.Context, conn string, bound []string) error
}

// ErrNotLeader is what a write of PollPositions wraps when the store refused it because this broker no longer leads: the poller stops.
var ErrNotLeader = errors.New("teams: the store refused the write: this broker no longer leads")
