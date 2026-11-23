package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/nitecon/amux/browserauth"
	"github.com/nitecon/amux/storage"
)

type createUserRequest struct {
	Username    string  `json:"username"`
	Password    string  `json:"password"`
	DisplayName *string `json:"display_name"`
	Role        *string `json:"role"`
}

type updateUserRequest struct {
	DisplayName *string         `json:"display_name"`
	Role        *string         `json:"role"`
	Disabled    json.RawMessage `json:"disabled"`
	Password    *string         `json:"password"`
}

// registerUserAdminRoutes registers the retained user-administration API.
// The handler is called by the session/auth integration when it owns mux setup.
func (a *api) registerUserAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/users", a.listUsers)
	mux.HandleFunc("POST /v1/users", a.createUser)
	mux.HandleFunc("PATCH /v1/users/{id}", a.updateUser)
	mux.HandleFunc("DELETE /v1/users/{id}", a.deleteUser)
}

func requireUserAdmin(w http.ResponseWriter, r *http.Request) bool {
	if authenticatedPrincipal(r.Context()) == bearerAPIKeyPrincipal {
		return true
	}
	if principal := authenticatedSecurePrincipal(r.Context()); principal != nil {
		if principal.role == "administrator" {
			return true
		}
		writeError(w, http.StatusForbidden, "administrator role required")
		return false
	}
	if authenticatedBrowserUser(r.Context()) != nil {
		return allowBrowserAdmin(w, r)
	}
	writeError(w, http.StatusUnauthorized, "authentication required")
	return false
}

// allowBrowserAdmin restricts signed browser sessions on authenticated routes
// that also serve native callers under their existing authorization policy.
func allowBrowserAdmin(w http.ResponseWriter, r *http.Request) bool {
	if user := authenticatedBrowserUser(r.Context()); user != nil && (!user.IsAdmin() || !user.IsActive()) {
		writeError(w, http.StatusForbidden, "administrator role required")
		return false
	}
	return true
}

func (a *api) listUsers(w http.ResponseWriter, r *http.Request) {
	if !requireUserAdmin(w, r) {
		return
	}
	users, err := a.store.Users(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot list users")
		return
	}
	views := make([]userView, 0, len(users))
	for _, user := range users {
		views = append(views, userViewFromAccount(user))
	}
	writeJSON(w, views)
}

func (a *api) createUser(w http.ResponseWriter, r *http.Request) {
	if !requireUserAdmin(w, r) {
		return
	}
	var request createUserRequest
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	role := storage.UserRoleMember
	if request.Role != nil && strings.TrimSpace(*request.Role) != "" {
		role = strings.ToLower(strings.TrimSpace(*request.Role))
	}
	hash, err := browserauth.HashPassword(request.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	displayName := ""
	if request.DisplayName != nil {
		displayName = *request.DisplayName
	}
	user, err := a.store.CreateUser(r.Context(), request.Username, displayName, role, hash)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, userViewFromAccount(*user))
}

func (a *api) updateUser(w http.ResponseWriter, r *http.Request) {
	if !requireUserAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var request updateUserRequest
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	var update storage.UserUpdate
	update.DisplayName = request.DisplayName
	if request.Role != nil && strings.TrimSpace(*request.Role) != "" {
		role := strings.ToLower(strings.TrimSpace(*request.Role))
		update.Role = &role
	}
	if request.Password != nil && strings.TrimSpace(*request.Password) != "" {
		hash, err := browserauth.HashPassword(strings.TrimSpace(*request.Password))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		update.PasswordHash = &hash
	}
	if request.Disabled != nil {
		disabled := userDisabledValue(request.Disabled)
		update.Disabled = &disabled
	}
	if actor := authenticatedBrowserUser(r.Context()); actor != nil && actor.ID == id {
		update.ActingUserID = &actor.ID
	}
	user, err := a.store.UpdateUser(r.Context(), id, update)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if user == nil {
		writeError(w, http.StatusBadRequest, "user not found")
		return
	}
	writeJSON(w, userViewFromAccount(*user))
}

func (a *api) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !requireUserAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var actingUserID *int64
	if actor := authenticatedBrowserUser(r.Context()); actor != nil {
		actingUserID = &actor.ID
	}
	changed, err := a.store.DeleteUserAs(r.Context(), id, actingUserID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, projectMessageResolutionResponse{Changed: changed})
}

func userDisabledValue(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value := value.(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "on", "1", "yes":
			return true
		default:
			return false
		}
	case float64:
		return math.Trunc(value) == value && value != 0 && value >= -0x1p63 && value < 0x1p63
	default:
		return false
	}
}
