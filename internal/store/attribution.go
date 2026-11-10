package store

import "context"

// AttributedRows is how many rows AttributeConnection gave a connection, by table.
type AttributedRows struct {
	// Outbox and Presses are the outbox rows and the presses that had no connection.
	Outbox, Presses int
}

// Total is the number of rows in all.
func (a AttributedRows) Total() int { return a.Outbox + a.Presses }

// AttributeBatch is how many rows of a table AttributeConnection changes in one transaction. The first start after an
// upgrade finds every row without a connection, and one statement over a large outbox could pass the statement timeout of
// a Postgres store, which would fail every start of the leader; a batch is bounded, so each statement is short whatever
// the table holds. A call that finds more than a batch makes several, one after the other.
const AttributeBatch = 1000

// Attribution attributes the rows that were written before the broker recorded connections.
type Attribution interface {
	// AttributeConnection gives connection to every outbox row and every press whose Connection is empty, and reports
	// how many it changed. It works in batches of AttributeBatch rows, each a transaction of its own (on Postgres, fenced
	// like any write); an error part way leaves the batches before it done, and the next call carries on. Those rows
	// were written by a broker that served one connection, which is the one the caller names: it is the broker's own. It
	// is idempotent, and a call that finds nothing to change changes nothing, so the leader calls it at every start: an
	// older image that wrote empty rows after a rollback is covered by the next start. ErrInvalid if connection is empty.
	AttributeConnection(ctx context.Context, connection string) (AttributedRows, error)
	// UnattributedRows counts the outbox rows and the presses whose Connection is empty, the rows AttributeConnection would
	// change, and changes nothing. It is a read, so it needs no leader and is not fenced. A broker with no connection of its
	// own has none to give them, and counts them to refuse to start. The count is the whole table's, not one batch's.
	UnattributedRows(ctx context.Context) (AttributedRows, error)
}
