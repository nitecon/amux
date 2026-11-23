package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// migrateNativeSourcePrincipals is migration 31. Null values preserve old
// source and offer rows as untrusted until an authenticated report is accepted.
func migrateNativeSourcePrincipals(ctx context.Context, tx *sql.Tx) error {
	for _, col := range []struct{ table, name, kind string }{
		{"amux_presence_sources", "principal", "TEXT"},
		{"amux_presence_sources", "principal_sequence", "INTEGER"},
		{"amux_assignment_offer_claims", "reporter_principal", "TEXT"},
	} {
		exists, err := sqliteTableHasColumn(ctx, tx, col.table, col.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE "+col.table+" ADD COLUMN "+col.name+" "+col.kind); err != nil {
				return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(31)"); err != nil {
		return fmt.Errorf("record native source principal migration: %w", err)
	}
	return nil
}

func validPresencePrincipal(principal string) bool {
	return principal != "" && len(principal) <= 128 && strings.TrimSpace(principal) == principal && strings.IndexFunc(principal, unicode.IsControl) < 0
}

// nativeSourcePrincipal returns a principal only for the exact current
// authenticated report sequence. This proves source ownership only; callers
// must separately verify the target is a current report_kind='native' member
// from this exact source and sequence before authorizing a native effect.
// Trusted in-process reports can advance the source cursor without advancing
// principal_sequence, invalidating old proof.
func nativeSourcePrincipal(ctx context.Context, q queryer, source string, sequence int64) (string, error) {
	var currentSequence int64
	var principal sql.NullString
	var principalSequence sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT sequence,principal,principal_sequence
		FROM amux_presence_sources WHERE source=?`, source).Scan(&currentSequence, &principal, &principalSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !principal.Valid || !principalSequence.Valid || currentSequence != sequence || principalSequence.Int64 != sequence {
		return "", nil
	}
	return principal.String, nil
}
