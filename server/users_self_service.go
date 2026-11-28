package server

import (
	"net/http"

	"github.com/nitecon/amux/auth"
	"github.com/nitecon/amux/auth/authn"
	account "github.com/nitecon/amux/identity/user"
)

type userView struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   int64  `json:"created_at"`
	LastLoginAt *int64 `json:"last_login_at"`
}

func userViewFromAccount(user account.Account) userView {
	return userView{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, Role: user.Role,
		Disabled: !user.IsActive(), CreatedAt: user.CreatedAt, LastLoginAt: user.LastLoginAt,
	}
}

type changeOwnPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (a *api) registerSelfServiceUserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/users/me", a.currentUser)
	mux.HandleFunc("POST /v1/users/me/password", a.changeOwnPassword)
}

func (a *api) currentUser(w http.ResponseWriter, r *http.Request) {
	user := auth.BrowserUser(r.Context())
	if user == nil {
		writeError(w, http.StatusNotFound, "no browser session on this request")
		return
	}
	// Rust derives this endpoint's view from the stamped session headers, which
	// intentionally contain identity and role only.
	writeJSON(w, userView{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, Role: user.Role})
}

func (a *api) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	var request changeOwnPasswordRequest
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	session := auth.BrowserUser(r.Context())
	if session == nil {
		writeError(w, http.StatusBadRequest, "password changes require a browser session")
		return
	}
	newHash, err := authn.HashPassword(request.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := a.store.User(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password change failed")
		return
	}
	if user == nil {
		writeError(w, http.StatusBadRequest, "user not found")
		return
	}
	if user.SessionEpoch != session.SessionEpoch {
		writeError(w, http.StatusConflict, "user credential or active state changed")
		return
	}
	if !authn.VerifyPassword(request.CurrentPassword, user.PasswordHash) {
		writeError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}
	changed, err := a.store.ChangeUserPassword(r.Context(), user.ID, session.SessionEpoch, user.PasswordHash, newHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password change failed")
		return
	}
	if !changed {
		writeError(w, http.StatusConflict, "user credential or active state changed")
		return
	}
	writeJSON(w, struct {
		Changed bool `json:"changed"`
	}{Changed: true})
}
