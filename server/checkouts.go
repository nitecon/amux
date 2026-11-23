package server

import (
	"net/http"
	"time"

	"github.com/nitecon/amux/storage"
)

func (a *api) registerCheckoutRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/checkouts", a.reportCheckouts)
	mux.HandleFunc("GET /v1/checkouts", a.checkoutOverview)
}

func (a *api) reportCheckouts(w http.ResponseWriter, r *http.Request) {
	var request *struct {
		Hostname  string                        `json:"hostname"`
		Checkouts *[]storage.CheckoutReportItem `json:"checkouts"`
	}
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	if request == nil || request.Checkouts == nil {
		writeError(w, http.StatusBadRequest, "checkout report is required")
		return
	}
	if err := a.store.ReportMachineCheckouts(r.Context(), storage.MachineCheckoutReport{Hostname: request.Hostname, Checkouts: *request.Checkouts}, time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid checkout report")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *api) checkoutOverview(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		writeError(w, http.StatusBadRequest, "checkout overview does not accept query parameters")
		return
	}
	projects, err := a.store.CheckoutOverviews(r.Context(), time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "checkout overview failed")
		return
	}
	writeJSON(w, projects)
}
