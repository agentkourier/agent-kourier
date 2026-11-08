package store

import "context"

// ConnectionServing records which chat connections this broker served, so that a restarted broker can tell which it stopped
// serving: it compares the recorded set with the one it serves now.
type ConnectionServing interface {
	// ServedConnections returns the recorded connections ("namespace/name"), sorted byte for byte; none if none are recorded.
	ServedConnections(ctx context.Context) ([]string, error)
	// ReplaceServedConnections makes the recorded set exactly connections, in one transaction: a connection that is not in it
	// is removed, a new one is added as served from now, and one that is in both keeps the time it has been served since. A
	// connection listed twice is one. An empty set clears the record. ErrInvalid if a connection is empty.
	ReplaceServedConnections(ctx context.Context, connections []string) error
}
