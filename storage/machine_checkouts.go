package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	projectid "github.com/nitecon/amux/identity/project"
)

const machineCheckoutsMigration = 36
const goMachineCheckoutsTable = "amux_go_machine_checkouts"

// MachineCheckout is display metadata for a checkout last reported by a host.
// Origin never participates in project binding or task routing.
type MachineCheckout struct {
	Hostname string  `json:"hostname"`
	Origin   *string `json:"origin"`
}

type CheckoutReportItem struct {
	Project     string  `json:"project"`
	Origin      *string `json:"origin"`
	Unavailable bool    `json:"unavailable,omitempty"`
}

type MachineCheckoutReport struct {
	Hostname  string               `json:"hostname"`
	Checkouts []CheckoutReportItem `json:"checkouts"`
}

func migrateMachineCheckouts(ctx context.Context, tx *sql.Tx) error {
	var hasLegacy bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='machine_checkouts')").Scan(&hasLegacy); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_go_machine_checkouts (
		hostname TEXT NOT NULL,
		project_ident TEXT NOT NULL,
		origin TEXT,
		seen_at INTEGER NOT NULL,
		unavailable INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(hostname, project_ident)
	);`); err != nil {
		return fmt.Errorf("migrate machine checkouts: %w", err)
	}
	if hasLegacy {
		hasUnavailable, err := sqliteTableHasColumn(ctx, tx, "machine_checkouts", "unavailable")
		if err != nil {
			return err
		}
		unavailable := "0"
		if hasUnavailable {
			unavailable = "unavailable"
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO amux_go_machine_checkouts(hostname,project_ident,origin,seen_at,unavailable)
			SELECT hostname,project_ident,origin,seen_at,`+unavailable+` FROM machine_checkouts`); err != nil {
			return fmt.Errorf("preserve legacy machine checkouts: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", machineCheckoutsMigration); err != nil {
		return fmt.Errorf("record machine checkouts migration: %w", err)
	}
	return nil
}

// ReportMachineCheckouts atomically replaces one host's retained report.
func (s *Store) ReportMachineCheckouts(ctx context.Context, report MachineCheckoutReport, now time.Time) error {
	hostname := strings.TrimSpace(report.Hostname)
	if hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+goMachineCheckoutsTable+" WHERE hostname=?", hostname); err != nil {
		return err
	}
	for _, checkout := range report.Checkouts {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO amux_go_machine_checkouts(hostname,project_ident,origin,seen_at,unavailable) VALUES(?,?,?,?,?)`,
			hostname, projectid.NormalizeIdent(checkout.Project), safeCheckoutOrigin(checkout.Origin), now.UnixMilli(), checkout.Unavailable); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// safeCheckoutOrigin removes URL credentials and query/fragment data before
// persisting or returning a raw clone origin. The remaining value is display
// metadata only; canonical project binding comes from the projects table.
func safeCheckoutOrigin(origin *string) *string {
	if origin == nil {
		return nil
	}
	raw := *origin
	if projectid.ValidateCloneSource(raw) != nil {
		return nil
	}
	safe, representable := projectid.SanitizeCloneOriginURL(raw)
	if !representable || safe != raw {
		// The wire report is admitted only when it already contains a safe,
		// transport-preserving origin; local display may scrub HTTP credentials.
		return nil
	}
	if !strings.Contains(raw, "://") && strings.Contains(raw, "@") {
		// Absolute local paths may contain @. For SCP syntax, preserve only the
		// conventional credential-free Git transport username.
		if filepath.IsAbs(raw) || strings.HasPrefix(raw, "git@") {
			return &raw
		}
		return nil
	}
	return &raw
}
