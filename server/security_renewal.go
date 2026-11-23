package server

import (
	"net/http"

	"github.com/nitecon/amux/storage"
)

func (a *api) renewSecurityCertificate(w http.ResponseWriter, r *http.Request) {
	peer := authenticatedSecurePrincipal(r.Context())
	if peer == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request *storage.RenewalRequest
	if !readJSONRequest(w, r, &request, 32<<10) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "renewal request is required")
		return
	}
	response, err := peer.enrollment.RenewCertificate(r.Context(), peer.certificate, *request)
	if err != nil {
		writeError(w, http.StatusForbidden, "renewal request denied")
		return
	}
	writeJSON(w, response)
}

func (a *api) activateSecurityCertificate(w http.ResponseWriter, r *http.Request) {
	peer := authenticatedSecurePrincipal(r.Context())
	if peer == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	node, err := peer.enrollment.ActivateCertificate(r.Context(), peer.certificate)
	if err != nil {
		writeError(w, http.StatusForbidden, "activation request denied")
		return
	}
	writeJSON(w, node)
}
