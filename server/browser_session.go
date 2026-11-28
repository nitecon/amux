package server

import (
	"net/http"

	"github.com/nitecon/amux/auth/authn"
)

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
		ctx, ok := authn.AuthenticateGateway(r, a.secret, a.store.User)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		clearGatewayIdentityHeaders(r)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func clearGatewayIdentityHeaders(r *http.Request) {
	r.Header.Del("X-Gateway-User")
	r.Header.Del("X-Gateway-User-Role")
	r.Header.Del("X-Authenticated-Principal")
}
