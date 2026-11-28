package authn

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nitecon/amux/auth"
	"github.com/nitecon/amux/identity/user"
)

// AuthenticateGateway verifies a bearer credential or a signed browser session.
func AuthenticateGateway(r *http.Request, secret string, lookup func(context.Context, int64) (*user.Account, error)) (context.Context, bool) {
	ctx := r.Context()
	if BearerValid(r.Header.Get("Authorization"), secret) {
		return auth.WithPrincipal(ctx, auth.BearerPrincipal), true
	}
	if !hasGatewayUIHeader(r.Header) {
		return ctx, false
	}
	token := browserSessionCookie(r.Header)
	userID, ok := BrowserSessionUserID(token)
	if !ok {
		return ctx, false
	}
	account, err := lookup(ctx, userID)
	if err != nil || account == nil || !VerifyBrowserSession(secret, *account, token, time.Now().UnixMilli()) {
		return ctx, false
	}
	validated := *account
	ctx = auth.WithBrowserUser(ctx, &validated)
	return auth.WithPrincipal(ctx, "ui-user:"+strconv.FormatInt(account.ID, 10)), true
}

func hasGatewayUIHeader(headers http.Header) bool {
	for name := range headers {
		if strings.EqualFold(name, "X-Gateway-UI") {
			return true
		}
	}
	return false
}

func browserSessionCookie(headers http.Header) string {
	for _, line := range headers.Values("Cookie") {
		for _, pair := range strings.Split(line, ";") {
			name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if ok && strings.TrimSpace(name) == "gw_session" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}
