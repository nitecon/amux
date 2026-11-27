package server

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
	"github.com/rs/zerolog/log"
)

const (
	bootstrapWindow = time.Minute
	bootstrapLimit  = 10
	bootstrapPeers  = 1024
)

type securePrincipalContextKey struct{}

type securePrincipal struct {
	enrollment  *storage.SecurityEnrollment
	certificate *x509.Certificate
	NodeID      string
	role        string
}

func authenticatedSecurePrincipal(ctx context.Context) *securePrincipal {
	principal, _ := ctx.Value(securePrincipalContextKey{}).(*securePrincipal)
	return principal
}

func (p *securePrincipal) valid(ctx context.Context) bool {
	if p == nil || p.enrollment == nil || p.certificate == nil || !validServerGeneration(ctx, time.Now()) {
		return false
	}
	_, err := p.enrollment.AuthorizeCertificate(ctx, p.certificate)
	return err == nil
}

type bootstrapAttempt struct {
	started time.Time
	count   int
}

func (a *api) allowBootstrap(ip net.IP, now time.Time) bool {
	if ip == nil {
		return false
	}
	key := ip.String()
	a.bootstrapMu.Lock()
	defer a.bootstrapMu.Unlock()
	for peer, attempt := range a.bootstrapAttempts {
		if now.Sub(attempt.started) >= bootstrapWindow {
			delete(a.bootstrapAttempts, peer)
		}
	}
	attempt, ok := a.bootstrapAttempts[key]
	if !ok {
		if len(a.bootstrapAttempts) >= bootstrapPeers {
			return false
		}
		attempt = bootstrapAttempt{started: now}
	}
	attempt.count++
	a.bootstrapAttempts[key] = attempt
	return attempt.count <= bootstrapLimit
}

func bootstrapRequest(method, path string) bool {
	if method == http.MethodPost && path == "/v1/enrollment/requests" {
		return true
	}
	if method != http.MethodGet {
		return false
	}
	id, ok := strings.CutPrefix(path, "/v1/enrollment/requests/")
	if !ok || strings.Contains(id, "/") {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func (a *api) serveSecure(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if bootstrapRequest(r.Method, r.URL.Path) {
		if !a.allowBootstrap(remoteIP(r.RemoteAddr), time.Now()) {
			writeError(w, http.StatusTooManyRequests, "enrollment request limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
		return
	}
	if !validServerGeneration(r.Context(), time.Now()) || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	certificate := r.TLS.PeerCertificates[0]
	verified := false
	for _, chain := range r.TLS.VerifiedChains {
		if len(chain) > 0 && chain[0].Equal(certificate) {
			verified = true
			break
		}
	}
	if !verified {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	node, err := a.securityEnrollment.AuthorizeCertificate(r.Context(), certificate)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if values := r.Header.Values("X-Agent-Instance-Id"); len(values) != 0 {
		if len(values) != 1 || strings.Contains(values[0], ",") || a.securityEnrollment.BindInstance(r.Context(), node.NodeID, values[0]) != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	peerIP := remoteIP(r.RemoteAddr)
	if strings.HasPrefix(r.URL.Path, "/v1/server/") &&
		(node.NodeID != a.localNodeID || node.Role != "administrator" || peerIP == nil || !peerIP.IsLoopback()) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	principal := &securePrincipal{
		enrollment: a.securityEnrollment, certificate: certificate, NodeID: node.NodeID, role: node.Role,
	}
	ctx := context.WithValue(r.Context(), authenticatedPrincipalContextKey{}, node.NodeID)
	ctx = context.WithValue(ctx, securePrincipalContextKey{}, principal)
	next.ServeHTTP(w, r.WithContext(ctx))
}

func (a *api) enroll(w http.ResponseWriter, r *http.Request) {
	var request *storage.EnrollmentRequest
	if !readJSONRequest(w, r, &request, 32<<10) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "enrollment request is required")
		return
	}
	response, err := a.securityEnrollment.Enroll(r.Context(), *request)
	if err != nil {
		var rejection storage.EnrollmentRejection
		code, message := "authorization_failed", "Request authorization failed"
		if errors.As(err, &rejection) && rejection.Message() != "" {
			code, message = rejection.Error(), rejection.Message()
		}
		log.Warn().Str("reason", code).Msg("Secure enrollment rejected")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]string{"error": code, "message": message})
		return
	}
	writeJSON(w, response)
}

func (a *api) enrollmentResult(w http.ResponseWriter, r *http.Request) {
	response, found, err := a.securityEnrollment.IssuanceResponse(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusRequestTimeout, "enrollment lookup interrupted")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid enrollment request ID")
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, response)
}

func (a *api) generateInvitation(w http.ResponseWriter, r *http.Request) {
	invitation, err := a.securityEnrollment.IssueInvitation(a.discoveryPort)
	if err != nil {
		writeError(w, http.StatusForbidden, "invitation generation failed")
		return
	}
	writeJSON(w, invitation)
}

func (a *api) copyInvitation(w http.ResponseWriter, r *http.Request) {
	invitation, err := a.securityEnrollment.CopyInvitation()
	if err != nil {
		writeError(w, http.StatusNotFound, "invitation unavailable")
		return
	}
	writeJSON(w, invitation)
}

func (a *api) invalidateInvitation(w http.ResponseWriter, r *http.Request) {
	if err := a.securityEnrollment.RevokeInvitation(); err != nil {
		writeError(w, http.StatusForbidden, "invitation invalidation failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) revokeSecurityNode(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	if nodeID == a.localNodeID {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if err := a.securityEnrollment.RevokeNode(r.Context(), nodeID); err != nil {
		writeError(w, http.StatusForbidden, "node revocation failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
