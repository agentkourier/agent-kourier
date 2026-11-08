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
