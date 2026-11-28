package authz

import (
	"errors"

	"github.com/nitecon/amux/auth"
	"github.com/nitecon/amux/identity/user"
)

var ErrAuthenticationRequired = errors.New("authentication required")
var ErrAdministratorRequired = errors.New("administrator role required")

type UserAdminCaller struct {
	PrincipalID string
	NodeRole    *string
	BrowserUser *user.Account
}

func RequireUserAdmin(caller UserAdminCaller) error {
	if caller.PrincipalID == auth.BearerPrincipal {
		return nil
	}
	if caller.NodeRole != nil {
		if *caller.NodeRole == "administrator" {
			return nil
		}
		return ErrAdministratorRequired
	}
	if caller.BrowserUser == nil {
		return ErrAuthenticationRequired
	}
	return AllowBrowserAdmin(caller.BrowserUser)
}

// AllowBrowserAdmin checks browser users on routes shared with native clients.
func AllowBrowserAdmin(account *user.Account) error {
	if account != nil && (!account.IsAdmin() || !account.IsActive()) {
		return ErrAdministratorRequired
	}
	return nil
}
