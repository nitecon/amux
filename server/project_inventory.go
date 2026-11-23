package server

import (
	"net/http"
	"time"
)

func (a *api) projectInventory(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		writeError(w, http.StatusBadRequest, "project inventory does not accept query parameters")
		return
	}
	projects, err := a.store.ProjectOverviews(r.Context(), time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project inventory failed")
		return
	}
	writeJSON(w, projects)
}
