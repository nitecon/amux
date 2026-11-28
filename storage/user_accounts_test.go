package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	account "github.com/nitecon/amux/identity/user"
)

func TestUserAccountsRetainRustRowsAndApplyAccountTransitions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rust-users.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE users (
 id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE,
 display_name TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('admin','member')),
 password_hash TEXT NOT NULL, session_epoch INTEGER NOT NULL DEFAULT 1,
 disabled_at INTEGER, created_at INTEGER NOT NULL, last_login_at INTEGER
);
INSERT INTO users(id,username,display_name,role,password_hash,session_epoch,created_at,last_login_at)
VALUES(17,'retained','Retained User','admin','$argon2id$synthetic',9,123,456);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	user, err := s.User(ctx, 17)
	if err != nil || user == nil || user.Username != "retained" || user.Role != account.RoleAdmin || user.PasswordHash != "$argon2id$synthetic" || user.SessionEpoch != 9 || user.CreatedAt != 123 || user.LastLoginAt == nil || *user.LastLoginAt != 456 {
		t.Fatalf("retained user=%+v err=%v", user, err)
	}
	encoded, err := json.Marshal(user)
	if err != nil || string(encoded) == "" || bytes.Contains(encoded, []byte("synthetic")) {
		t.Fatalf("serialized account leaked password hash: %s, %v", encoded, err)
	}

	created, err := s.CreateUser(ctx, "  New.User ", "  New User  ", account.RoleMember, "$argon2id$member")
	if err != nil || created.Username != "new.user" || created.DisplayName != "New User" || created.SessionEpoch != 1 {
		t.Fatalf("created user=%+v err=%v", created, err)
	}
	if _, err := s.CreateUser(ctx, "NEW.USER", "", account.RoleMember, "hash"); err == nil {
		t.Fatal("case-insensitive duplicate username accepted")
	}
	for _, input := range []struct{ username, role string }{{"bad name", account.RoleMember}, {"x", account.RoleMember}, {"valid", "root"}} {
		if _, err := s.CreateUser(ctx, input.username, "", input.role, "hash"); !errors.Is(err, account.ErrInvalid) {
			t.Fatalf("invalid user %+v err=%v", input, err)
		}
	}
	count, err := s.UserCount(ctx)
	if err != nil || count != 2 {
		t.Fatalf("user count=%d err=%v", count, err)
	}
	users, err := s.Users(ctx)
	if err != nil || len(users) != 2 || users[0].Username != "new.user" || users[1].Username != "retained" {
		t.Fatalf("ordered users=%+v err=%v", users, err)
	}
	if found, err := s.UserByUsername(ctx, " RETAINED "); err != nil || found == nil || found.ID != 17 {
		t.Fatalf("username lookup=%+v err=%v", found, err)
	}

	epoch := created.SessionEpoch
	updated, err := s.UpdateUser(ctx, created.ID, UserUpdate{PasswordHash: userTestStringPtr("$argon2id$member")})
	if err != nil || updated.SessionEpoch != epoch+1 {
		t.Fatalf("same-hash password operation epoch=%v err=%v", updated, err)
	}
	verifiedEpoch := updated.SessionEpoch
	updated, err = s.UpdateUser(ctx, created.ID, UserUpdate{PasswordHash: userTestStringPtr("$argon2id$next"), Disabled: userTestBoolPtr(true)})
	if err != nil || updated.IsActive() || updated.SessionEpoch != epoch+3 {
		t.Fatalf("password+disable transition=%+v err=%v", updated, err)
	}
	disabledEpoch := updated.SessionEpoch
	updated, err = s.UpdateUser(ctx, created.ID, UserUpdate{Disabled: userTestBoolPtr(false)})
	if err != nil || !updated.IsActive() || updated.SessionEpoch != disabledEpoch {
		t.Fatalf("reenable transition=%+v err=%v", updated, err)
	}
	if changed, err := s.ChangeUserPassword(ctx, created.ID, verifiedEpoch, "$argon2id$next", "$argon2id$stale"); err != nil || changed {
		t.Fatalf("stale epoch password CAS changed=%v err=%v", changed, err)
	}
	expectedCurrentEpoch := updated.SessionEpoch
	if changed, err := s.ChangeUserPassword(ctx, created.ID, expectedCurrentEpoch, "$argon2id$next", "$argon2id$final"); err != nil || !changed {
		t.Fatalf("current password CAS changed=%v err=%v", changed, err)
	}
	updated, err = s.User(ctx, created.ID)
	if err != nil || updated.PasswordHash != "$argon2id$final" || updated.SessionEpoch != expectedCurrentEpoch+1 {
		t.Fatalf("password CAS result=%+v err=%v", updated, err)
	}
	if err := s.TouchUserLogin(ctx, updated.ID); err != nil {
		t.Fatal(err)
	}
	updated, err = s.User(ctx, updated.ID)
	if err != nil || updated.LastLoginAt == nil {
		t.Fatalf("login timestamp was not retained: %+v err=%v", updated, err)
	}
	if admins, err := s.CountActiveAdmins(ctx); err != nil || admins != 1 {
		t.Fatalf("active admins=%d err=%v", admins, err)
	}
	if _, err := s.UpdateUser(ctx, 17, UserUpdate{Role: userTestStringPtr(account.RoleMember)}); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("last admin demotion err=%v", err)
	}
	if changed, err := s.DeleteUser(ctx, 17); changed || !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("last admin delete changed=%v err=%v", changed, err)
	}
	if admins, err := s.CountActiveAdmins(ctx); err != nil || admins != 1 {
		t.Fatalf("admin invariant lost: count=%d err=%v", admins, err)
	}

	promoted, err := s.UpdateUser(ctx, created.ID, UserUpdate{DisplayName: userTestStringPtr("Promoted User"), Role: userTestStringPtr(account.RoleAdmin)})
	if err != nil || promoted == nil || !promoted.IsAdmin() || promoted.DisplayName != "Promoted User" {
		t.Fatalf("profile promotion: %+v err=%v", promoted, err)
	}
	if admins, err := s.CountActiveAdmins(ctx); err != nil || admins != 2 {
		t.Fatalf("active admin count after promotion=%d err=%v", admins, err)
	}
	if changed, err := s.DeleteUser(ctx, created.ID); err != nil || !changed {
		t.Fatalf("delete promoted user: changed=%t err=%v", changed, err)
	}
	if changed, err := s.DeleteUser(ctx, created.ID); err != nil || changed {
		t.Fatalf("repeat delete: changed=%t err=%v", changed, err)
	}

}

func TestUserAccountsConcurrentDeletePreservesLastActiveAdmin(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if count, err := s.UserCount(ctx); err != nil || count != 0 {
		t.Fatalf("fresh user count=%d err=%v", count, err)
	}
	one, err := s.CreateUser(ctx, "admin-one", "", account.RoleAdmin, "hash-one")
	if err != nil {
		t.Fatal(err)
	}
	if one == nil || one.DisplayName != "admin-one" {
		t.Fatalf("empty display name should default to username: %+v", one)
	}
	two, err := s.CreateUser(ctx, "admin-two", "", account.RoleAdmin, "hash-two")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []int64{one.ID, two.ID} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			_, err := s.DeleteUser(ctx, id)
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, blocked := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrLastActiveAdmin):
			blocked++
		default:
			t.Fatalf("concurrent delete error=%v", err)
		}
	}
	admins, err := s.CountActiveAdmins(ctx)
	if err != nil || successes != 1 || blocked != 1 || admins != 1 {
		t.Fatalf("deletes success=%d blocked=%d admins=%d err=%v", successes, blocked, admins, err)
	}
}

func userTestStringPtr(value string) *string { return &value }
func userTestBoolPtr(value bool) *bool       { return &value }
