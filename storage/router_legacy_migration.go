package storage

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

const currentRouterDeliveryStateCheck = `CHECK(state IN ('pending','running','retrying','completed','stopped','skipped','failed'))`

var legacyRouterDeliveryStatePattern = regexp.MustCompile(`(?i:CHECK\s*\(\s*state\s+IN\s*\()\s*('pending'\s*,\s*'running'\s*,\s*'retrying'\s*,\s*'completed'\s*,\s*'stopped'\s*,\s*'skipped')\s*\)\s*\)`)
var currentRouterDeliveryStatePattern = regexp.MustCompile(`(?i:CHECK\s*\(\s*state\s+IN\s*\()\s*('pending'\s*,\s*'running'\s*,\s*'retrying'\s*,\s*'completed'\s*,\s*'stopped'\s*,\s*'skipped'\s*,\s*'failed')\s*\)\s*\)`)
var routerDeliveryStateConstraintPattern = regexp.MustCompile(`(?i)CHECK\s*\(\s*state\b`)

// migrateLegacyRouterDeliveries widens only the recognized legacy router state
// check. The backup uses the established task-status prefix for compatibility
// with the shared protected SQLite backup implementation.
func migrateLegacyRouterDeliveries(ctx context.Context, db *sql.DB, path string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get SQLite connection: %w", err)
	}
	defer conn.Close()

	var schema sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='router_deliveries'").Scan(&schema); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("read router_deliveries schema: %w", err)
	}
	if !schema.Valid {
		return fmt.Errorf("router_deliveries table has no stored schema")
	}
	ddl := schema.String

	stateChecks := routerDeliveryStateConstraintPattern.FindAllStringIndex(ddl, -1)
	if len(stateChecks) != 1 {
		return fmt.Errorf("unrecognized router_deliveries state constraint; refusing router migration")
	}
	if current := currentRouterDeliveryStatePattern.FindStringSubmatch(ddl); current != nil && compactSQL(current[1]) == compactSQL("'pending','running','retrying','completed','stopped','skipped','failed'") {
		return nil
	}
	match := legacyRouterDeliveryStatePattern.FindStringSubmatchIndex(ddl)
	if match == nil || compactSQL(ddl[match[2]:match[3]]) != compactSQL("'pending','running','retrying','completed','stopped','skipped'") {
		return fmt.Errorf("unrecognized router_deliveries state constraint; refusing router migration")
	}

	if _, _, err := createTaskStatusBackup(ctx, conn, path); err != nil {
		return err
	}
	newDDL := ddl[:match[0]] + currentRouterDeliveryStateCheck + ddl[match[1]:]
	newDDL, err = replaceCreateTableName(newDDL, "router_deliveries", "router_deliveries_state_rebuild")
	if err != nil {
		return err
	}
	return rebuildLegacyTable(ctx, conn, "router_deliveries", "router_deliveries_state_rebuild", newDDL)
}
