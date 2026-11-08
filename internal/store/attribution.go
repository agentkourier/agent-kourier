package store

import "context"

// AttributedRows is how many rows AttributeConnection gave a connection, by table.
type AttributedRows struct {
	// Outbox and Presses are the outbox rows and the presses that had no connection.
	Outbox, Presses int
}

// Total is the number of rows in all.
func (a AttributedRows) Total() int { return a.Outbox + a.Presses }

// Attribution attributes the rows that were written before the broker recorded connections.
type Attribution interface {
	// AttributeConnection gives connection to every outbox row and every press whose Connection is empty, and reports
	// how many it changed. Those rows were written by a broker that served one connection, which is the one the caller
	// names: it is the broker's own. It is idempotent, and a call that finds nothing to change changes nothing, so the
	// leader calls it at every start: an older image that wrote empty rows after a rollback is covered by the next
	// start. ErrInvalid if connection is empty.
	AttributeConnection(ctx context.Context, connection string) (AttributedRows, error)
}
