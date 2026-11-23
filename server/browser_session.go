package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nitecon/amux/storage"
)

type browserUserContextKey struct{}

func authenticatedBrowserUser(ctx context.Context) *storage.User {
	user, _ := ctx.Value(browserUserContextKey{}).(*storage.User)
	return user
}

func (a *api) withGatewayAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.securityEnrollment != nil && bootstrapRequest(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if peer := authenticatedSecurePrincipal(r.Context()); peer != nil {
			clearGatewayIdentityHeaders(r)
			next.ServeHTTP(w, r)
			return
		}
		if gatewayBearerValid(r.Header.Get("Authorization"), a.secret) {
			clearGatewayIdentityHeaders(r)
			ctx := context.WithValue(r.Context(), authenticatedPrincipalContextKey{}, bearerAPIKeyPrincipal)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if !hasGatewayUIHeader(r.Header) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		token := browserSessionCookie(r.Header)
		userID, ok := browserSessionTokenUserID(token)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		user, err := a.store.User(r.Context(), userID)
		if err != nil || user == nil || !verifyBrowserSessionToken(a.secret, *user, token, nowMilliseconds()) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		clearGatewayIdentityHeaders(r)
		validated := *user
		ctx := context.WithValue(r.Context(), browserUserContextKey{}, &validated)
		ctx = context.WithValue(ctx, authenticatedPrincipalContextKey{}, "ui-user:"+strconv.FormatInt(user.ID, 10))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func gatewayBearerValid(header, secret string) bool {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	want := sha256.Sum256([]byte(secret))
	supplied := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(supplied[:], want[:]) == 1
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

func clearGatewayIdentityHeaders(r *http.Request) {
	r.Header.Del("X-Gateway-User")
	r.Header.Del("X-Gateway-User-Role")
	r.Header.Del("X-Authenticated-Principal")
}

func nowMilliseconds() int64 { return time.Now().UnixMilli() }
