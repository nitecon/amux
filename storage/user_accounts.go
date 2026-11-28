package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	account "github.com/nitecon/amux/identity/user"
)

const (
	userAccountsMigration = 46
)

var ErrLastActiveAdmin = errors.New("cannot remove the last active administrator")
var ErrSelfUserMutation = errors.New("you cannot demote, disable or delete your own account")

// UserUpdate applies the profile and credential changes from one account
// update atomically. Password and disable transitions advance session_epoch.
type UserUpdate struct {
	DisplayName  *string
	Role         *string
	PasswordHash *string
	Disabled     *bool
	ActingUserID *int64
}

func migrateUserAccounts(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 username TEXT NOT NULL UNIQUE,
 display_name TEXT NOT NULL,
 role TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('admin','member')),
 password_hash TEXT NOT NULL,
 session_epoch INTEGER NOT NULL DEFAULT 1,
 disabled_at INTEGER,
 created_at INTEGER NOT NULL,
 last_login_at INTEGER
);
INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(46);`); err != nil {
		return fmt.Errorf("migrate user accounts: %w", err)
	}
	return nil
}

const userColumns = `id,username,display_name,role,password_hash,session_epoch,disabled_at,created_at,last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*account.Account, error) {
	var user account.Account
	if err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &user.PasswordHash, &user.SessionEpoch, &user.DisabledAt, &user.CreatedAt, &user.LastLoginAt); err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Store) UserCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

func (s *Store) Users(ctx context.Context) ([]account.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]account.Account, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *user)
	}
	return users, rows.Err()
}

func (s *Store) User(ctx context.Context, id int64) (*account.Account, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return user, err
}

func (s *Store) UserByUsername(ctx context.Context, username string) (*account.Account, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	user, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE username=?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return user, err
}

func (s *Store) CreateUser(ctx context.Context, username, displayName, role, passwordHash string) (*account.Account, error) {
	username, err := account.NormalizeUsername(username)
	if err != nil {
		return nil, err
	}
	if role != account.RoleAdmin && role != account.RoleMember {
		return nil, fmt.Errorf("%w: role must be 'admin' or 'member'", account.ErrInvalid)
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = username
	}
	if strings.TrimSpace(passwordHash) == "" {
		return nil, fmt.Errorf("%w: password hash is required", account.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO users(username,display_name,role,password_hash,session_epoch,created_at) VALUES(?,?,?,?,1,?)`, username, displayName, role, passwordHash, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.User(ctx, id)
}

func (s *Store) UpdateUser(ctx context.Context, id int64, update UserUpdate) (*account.Account, error) {
	if update.Role != nil && *update.Role != account.RoleAdmin && *update.Role != account.RoleMember {
		return nil, fmt.Errorf("%w: role must be 'admin' or 'member'", account.ErrInvalid)
	}
	if update.PasswordHash != nil && strings.TrimSpace(*update.PasswordHash) == "" {
		return nil, fmt.Errorf("%w: password hash is required", account.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	name := current.DisplayName
	if update.DisplayName != nil && strings.TrimSpace(*update.DisplayName) != "" {
		name = strings.TrimSpace(*update.DisplayName)
	}
	role := current.Role
	if update.Role != nil {
		role = *update.Role
	}
	passwordHash := current.PasswordHash
	if update.PasswordHash != nil {
		passwordHash = *update.PasswordHash
	}
	disabled := current.DisabledAt != nil
	if update.Disabled != nil {
		disabled = *update.Disabled
	}
	if current.IsAdmin() && current.IsActive() && (role != account.RoleAdmin || disabled) {
		var activeAdmins int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND disabled_at IS NULL`).Scan(&activeAdmins); err != nil {
			return nil, err
		}
		if activeAdmins <= 1 {
			return nil, ErrLastActiveAdmin
		}
	}
	if update.ActingUserID != nil && *update.ActingUserID == id &&
		((role == account.RoleMember && current.IsAdmin()) || (current.IsActive() && disabled)) {
		return nil, ErrSelfUserMutation
	}
	var disabledAt any
	if disabled {
		if current.DisabledAt != nil {
			disabledAt = *current.DisabledAt
		} else {
			disabledAt = time.Now().UnixMilli()
		}
	}
	epochDelta := 0
	if update.PasswordHash != nil {
		epochDelta++
	}
	if current.DisabledAt == nil && disabled {
		epochDelta++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name=?,role=?,password_hash=?,disabled_at=?,session_epoch=session_epoch+? WHERE id=?`, name, role, passwordHash, disabledAt, epochDelta, id); err != nil {
		return nil, err
	}
	updated, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// ChangeUserPassword performs the password verification's write half with a
// compare-and-swap, so a concurrent password change or disable cannot be
// overwritten after the caller checked the old password.
func (s *Store) ChangeUserPassword(ctx context.Context, id, expectedEpoch int64, expectedHash, newHash string) (bool, error) {
	if strings.TrimSpace(expectedHash) == "" || strings.TrimSpace(newHash) == "" {
		return false, fmt.Errorf("%w: password hash is required", account.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=?,session_epoch=session_epoch+1 WHERE id=? AND session_epoch=? AND password_hash=? AND disabled_at IS NULL`, newHash, id, expectedEpoch, expectedHash)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed != 0, err
}

func (s *Store) TouchUserLogin(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at=? WHERE id=?`, time.Now().UnixMilli(), id)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id int64) (bool, error) {
	return s.DeleteUserAs(ctx, id, nil)
}

func (s *Store) DeleteUserAs(ctx context.Context, id int64, actingUserID *int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if actingUserID != nil && *actingUserID == id {
		return false, ErrSelfUserMutation
	}
	user, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if user.IsAdmin() && user.IsActive() {
		var activeAdmins int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND disabled_at IS NULL`).Scan(&activeAdmins); err != nil {
			return false, err
		}
		if activeAdmins <= 1 {
			return false, ErrLastActiveAdmin
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed != 0, nil
}

func (s *Store) CountActiveAdmins(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND disabled_at IS NULL`).Scan(&count)
	return count, err
}
