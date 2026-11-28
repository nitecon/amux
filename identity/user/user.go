// Package user defines gateway user accounts.
package user

import (
	"errors"
	"fmt"
	"strings"
)

const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

var ErrInvalid = errors.New("invalid user")

// Account retains the gateway account record. PasswordHash is storage-only.
type Account struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	Role         string `json:"role"`
	PasswordHash string `json:"-"`
	SessionEpoch int64  `json:"session_epoch"`
	DisabledAt   *int64 `json:"disabled_at"`
	CreatedAt    int64  `json:"created_at"`
	LastLoginAt  *int64 `json:"last_login_at"`
}

func (u Account) IsAdmin() bool  { return u.Role == RoleAdmin }
func (u Account) IsActive() bool { return u.DisabledAt == nil }

func NormalizeUsername(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if len(name) < 2 || len(name) > 40 {
		return "", fmt.Errorf("%w: username must be 2-40 characters", ErrInvalid)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return "", fmt.Errorf("%w: username may contain letters, digits, '.', '_' and '-' only", ErrInvalid)
		}
	}
	return name, nil
}
