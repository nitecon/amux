package storage

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

var nativePresenceProjectReference = regexp.MustCompile(`(?i)\b(project_ident\s+TEXT\s+NOT\s+NULL)\s+REFERENCES\s+projects\s*\(\s*ident\s*\)\s+ON\s+DELETE\s+CASCADE\b`)

// migrateLegacyPresenceAuthority permits unresolved event observations in the
// canonical table. Native report validation still requires a registered project.
// Only the Go-owned, recognized project reference is removed; other constraints,
// child references and owned schema objects survive the protected rebuild.
func migrateLegacyPresenceAuthority(ctx context.Context, db *sql.DB, path string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var ddl string
	err = conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='amux_presence_agents'").Scan(&ddl)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_list('amux_presence_agents')").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	var table, from, to, deletion, update string
	if count != 1 {
		return fmt.Errorf("unrecognized presence foreign keys; refusing authority migration")
	}
	if err := conn.QueryRowContext(ctx, `SELECT "table","from","to",on_delete,on_update FROM pragma_foreign_key_list('amux_presence_agents')`).Scan(&table, &from, &to, &deletion, &update); err != nil {
		return err
	}
	if table != "projects" || from != "project_ident" || to != "ident" || deletion != "CASCADE" || update != "NO ACTION" || len(nativePresenceProjectReference.FindAllStringIndex(ddl, -1)) != 1 {
		return fmt.Errorf("unrecognized presence project reference; refusing authority migration")
	}
	newDDL := nativePresenceProjectReference.ReplaceAllString(ddl, "${1}")
	newDDL, err = replaceCreateTableName(newDDL, "amux_presence_agents", "amux_presence_authority_rebuild")
	if err != nil {
		return err
	}
	if _, _, err := createTaskStatusBackup(ctx, conn, path); err != nil {
		return err
	}
	return rebuildLegacyTable(ctx, conn, "amux_presence_agents", "amux_presence_authority_rebuild", newDDL)
}
