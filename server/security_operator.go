package server

import (
	"net/http"
	"os"

	"github.com/nitecon/amux/storage"
)

type securityServerStatus struct {
	ClusterID           string                           `json:"cluster_id"`
	Running             bool                             `json:"running"`
	ProcessID           int                              `json:"process_id"`
	ProcessNonce        string                           `json:"process_nonce"`
	SecureBind          string                           `json:"secure_bind"`
	DiscoveryBind       string                           `json:"discovery_bind"`
	ServerNames         []string                         `json:"server_names"`
	Nodes               []storage.AuthorizedSecurityNode `json:"nodes"`
	ServerSerial        string                           `json:"server_serial"`
	ServerExpiresAt     int64                            `json:"server_expires_at"`
	PreviousSerial      *string                          `json:"previous_server_serial"`
	PreviousServerUntil *int64                           `json:"previous_server_until"`
}

func (a *api) serverStatus(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.securityEnrollment.SecurityNodes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read security node status")
		return
	}
	identity, err := a.securityEnrollment.ServerIdentityStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read server certificate status")
		return
	}
	writeJSON(w, securityServerStatus{
		ClusterID: identity.ClusterID, Running: true, ProcessID: os.Getpid(), ProcessNonce: a.processNonce,
		SecureBind: a.secureBind, DiscoveryBind: a.discoveryBind, ServerNames: identity.Names,
		Nodes: nodes, ServerSerial: identity.Serial, ServerExpiresAt: identity.ExpiresAt,
		PreviousSerial: identity.PreviousSerial, PreviousServerUntil: identity.PreviousExpiresAt,
	})
}

func (a *api) serverShutdown(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedProcessNonce string `json:"expected_process_nonce"`
	}
	if !readJSONRequest(w, r, &request, 4096) {
		return
	}
	if request.ExpectedProcessNonce != a.processNonce {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if a.shutdownStarted.CompareAndSwap(false, true) {
		if err := a.securityEnrollment.BeginShutdown(r.Context()); err != nil {
			a.shutdownStarted.Store(false)
			writeError(w, http.StatusInternalServerError, "cannot begin server shutdown")
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		go a.shutdown()
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
