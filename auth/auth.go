// Package auth carries authenticated gateway identities in request contexts.
package auth

import (
	"context"

	"github.com/nitecon/amux/identity/user"
)

const BearerPrincipal = "api-key"

type principalKey struct{}
type browserUserKey struct{}

func WithPrincipal(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, principalKey{}, id)
}

func PrincipalID(ctx context.Context) string {
	id, _ := ctx.Value(principalKey{}).(string)
	return id
}

func WithBrowserUser(ctx context.Context, account *user.Account) context.Context {
	return context.WithValue(ctx, browserUserKey{}, account)
}

func BrowserUser(ctx context.Context) *user.Account {
	account, _ := ctx.Value(browserUserKey{}).(*user.Account)
	return account
}
