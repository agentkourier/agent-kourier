package broker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// attributeStoredRows gives the broker's own connection to the outbox rows and presses that have none, which were written
// before the broker recorded connections: every one of them was the own connection's, as the broker served only that. It
// runs at every leader start, before the outbox reconciles and the sessions recover, so that nothing reads a row that
// belongs to nobody; an older image that wrote such rows after a rollback is covered by the next start. It logs at INFO what
// it changed, and nothing when it changed nothing, which is every start after the first.
func attributeStoredRows(ctx context.Context, st store.Attribution, log *slog.Logger, own string) error {
	rows, err := st.AttributeConnection(ctx, own)
	if err != nil {
		return fmt.Errorf("attribute the stored outbox rows and presses to connection %s: %w", own, err)
	}
	if rows.Total() > 0 {
		log.InfoContext(ctx, "store: gave the rows written before connections were recorded to this broker's own connection",
			"connection", own, "outbox_rows", rows.Outbox, "presses", rows.Presses)
	}
	return nil
}

// refuseUnattributedRows is what a leader with no own connection does where attributeStoredRows would give the rows that have
// no connection to the own one: it counts them, and if there are any it returns an error wrapping ErrWiring, which ends the
// process with exit 2, since no restart finds another answer. Those rows were written by a broker that served one
// connection, and only the operator knows which; setting config.chatConnection to it for one start gives them that owner
// (attributeStoredRows), and unsetting it afterwards leaves none. The message holds the two counts and no row. A store that
// cannot be read is not that: the error is the store's and a restart may mend it.
func refuseUnattributedRows(ctx context.Context, st store.Attribution) error {
	rows, err := st.UnattributedRows(ctx)
	if err != nil {
		return fmt.Errorf("count the stored outbox rows and presses that have no connection: %w", err)
	}
	if rows.Total() == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d outbox rows and %d button presses were written before connections were recorded, and this broker has no ChatConnection of its own to give them to: "+
		"set config.chatConnection (AGENTKOURIER_CHAT_CONNECTION) to the ChatConnection that served them, let the broker start once, then unset it",
		ErrWiring, rows.Outbox, rows.Presses)
}
