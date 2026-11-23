package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// Keep receipt deduplication while either journal replay or an unfinished
// native command can still refer to its effect.
func pruneNativeDeliveryReceiptsIn(ctx context.Context, tx *sql.Tx, cutoff int64) error {
	present, err := sqliteTableHasColumn(ctx, tx, "amux_native_delivery_receipts", "notify_uid")
	if err != nil || !present {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM amux_native_delivery_receipts
		WHERE status IN ('completed','skipped') AND updated_at<?
		AND NOT EXISTS(SELECT 1 FROM router_events e WHERE e.uid=amux_native_delivery_receipts.notify_uid)
		AND NOT EXISTS(SELECT 1 FROM amux_conversation_commands c
			WHERE c.conversation_id=amux_native_delivery_receipts.conversation_id
			AND c.id=amux_native_delivery_receipts.command_id AND c.state IN ('queued','claimed'))`, cutoff)
	if err != nil {
		return fmt.Errorf("prune native delivery receipts: %w", err)
	}
	return nil
}
