package storage

import (
	"context"
	"database/sql"
)

const skillCatalogMigration = 44

type SkillMeta struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	Checksum   string `json:"checksum"`
	UploadedAt int64  `json:"uploaded_at"`
}

type SkillRecord struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	ZipData    []byte  `json:"-"`
	Content    *string `json:"content"`
	Size       int64   `json:"size"`
	Checksum   string  `json:"checksum"`
	UploadedAt int64   `json:"uploaded_at"`
}

func migrateSkills(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS skills (
 name TEXT PRIMARY KEY,zip_data BLOB NOT NULL,size INTEGER NOT NULL,
 checksum TEXT NOT NULL,uploaded_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	for _, column := range []struct{ name, ddl string }{
		{"kind", "TEXT NOT NULL DEFAULT 'skill'"},
		{"content", "TEXT"},
	} {
		present, err := sqliteTableHasColumn(ctx, tx, "skills", column.name)
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE skills ADD COLUMN "+column.name+" "+column.ddl); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", skillCatalogMigration)
	return err
}

func (s *Store) ListSkills(ctx context.Context, kind *string) ([]SkillMeta, error) {
	query := `SELECT name,kind,size,checksum,uploaded_at FROM skills ORDER BY kind ASC,name ASC`
	var rows *sql.Rows
	var err error
	if kind == nil {
		rows, err = s.db.QueryContext(ctx, query)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT name,kind,size,checksum,uploaded_at FROM skills WHERE kind=? ORDER BY name ASC`, *kind)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]SkillMeta, 0)
	for rows.Next() {
		var item SkillMeta
		if err := rows.Scan(&item.Name, &item.Kind, &item.Size, &item.Checksum, &item.UploadedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) Skill(ctx context.Context, name string) (*SkillRecord, error) {
	var item SkillRecord
	err := s.db.QueryRowContext(ctx, `SELECT name,kind,zip_data,content,size,checksum,uploaded_at FROM skills WHERE name=?`, name).
		Scan(&item.Name, &item.Kind, &item.ZipData, &item.Content, &item.Size, &item.Checksum, &item.UploadedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Store) UpsertSkill(ctx context.Context, item SkillRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO skills(name,zip_data,kind,content,size,checksum,uploaded_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET
zip_data=excluded.zip_data,kind=excluded.kind,content=excluded.content,size=excluded.size,
checksum=excluded.checksum,uploaded_at=excluded.uploaded_at`,
		item.Name, item.ZipData, item.Kind, item.Content, item.Size, item.Checksum, item.UploadedAt)
	return err
}

func (s *Store) DeleteSkill(ctx context.Context, name string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM skills WHERE name=?", name)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}
