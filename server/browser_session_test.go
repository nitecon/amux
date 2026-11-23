package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/browserauth"
	"github.com/nitecon/amux/storage"
)

const browserSessionTestAPIKey = "synthetic-browser-session-api-key"

func TestBrowserSessionEventAndHistoryReadsRequireAdministrator(t *testing.T) {
	store, srv, user := browserSessionTestFixture(t)
	token := mintBrowserSessionToken(browserSessionTestAPIKey, *user, time.Now().Add(time.Hour).UnixMilli())
	paths := []string{"/v1/task-events", "/v1/task-events/consumers", "/v1/execution/runs"}
	for _, path := range paths {
		response, body := browserSessionRequest(t, srv, http.MethodGet, path, "", token, "", true)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("member read %s status=%d body=%s", path, response.StatusCode, body)
		}
	}
	role := storage.UserRoleAdmin
	if _, err := store.UpdateUser(context.Background(), user.ID, storage.UserUpdate{Role: &role}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		response, body := browserSessionRequest(t, srv, http.MethodGet, path, "", token, "", true)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("administrator read %s status=%d body=%s", path, response.StatusCode, body)
		}
	}
}

func TestBrowserSessionAttributesSingleAndBulkMessageResolution(t *testing.T) {
	store, srv, user := browserSessionTestFixture(t)
	ctx := context.Background()
	const project = "browser-resolution"
	if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: "unconfigured", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	token := mintBrowserSessionToken(browserSessionTestAPIKey, *user, time.Now().Add(time.Hour).UnixMilli())
	root := "/v1/projects/" + project + "/messages"
	response, body := browserSessionRequest(t, srv, http.MethodPost, root+"/human", `{"content":"hello agents"}`, token, "", true)
	var single humanProjectMessageResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &single) != nil || single.Delivered || single.DeliveryError == nil {
		t.Fatalf("unavailable channel post status=%d body=%s", response.StatusCode, body)
	}
	bulk, err := store.CreateHumanProjectMessage(ctx, project, storage.HumanProjectMessageInput{Content: "bulk"})
	if err != nil {
		t.Fatal(err)
	}
	response, body = browserSessionRequest(t, srv, http.MethodPost, fmt.Sprintf("%s/%d/resolve", root, single.MessageID), "", token, "", true)
	var resolved projectMessageResolutionResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &resolved) != nil || !resolved.Changed {
		t.Fatalf("single resolution status=%d body=%s", response.StatusCode, body)
	}
	assertMessageResolution(t, store, project, single.MessageID, true, "user:"+user.Username)
	payload := fmt.Sprintf(`{"before_ms":%d}`, bulk.SentAt+1)
	response, body = browserSessionRequest(t, srv, http.MethodPost, root+"/resolve", payload, token, "", true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("bulk resolution status=%d body=%s", response.StatusCode, body)
	}
	assertMessageResolution(t, store, project, bulk.ID, true, "user:"+user.Username)
}

func browserSessionTestFixture(t *testing.T) (*storage.Store, *httptest.Server, *storage.User) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "browser-session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hash, err := browserauth.HashPassword("synthetic current password")
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(ctx, "session-member", "Session Member", storage.UserRoleMember, hash)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, browserSessionTestAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return store, srv, user
}

func browserSessionRequest(t *testing.T, srv *httptest.Server, method, path, body, token, auth string, marked bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Cookie", "other=1; gw_session="+token)
	}
	if marked {
		req.Header["X-Gateway-UI"] = []string{""}
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Gateway-User", "999:forged:Administrator")
	req.Header.Set("X-Gateway-User-Role", "admin")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res, responseBody
}

func TestBrowserSessionIngressRequiresMarkedRequestAndPreservesBearerPriority(t *testing.T) {
	_, srv, user := browserSessionTestFixture(t)
	token := mintBrowserSessionToken(browserSessionTestAPIKey, *user, time.Now().Add(time.Hour).UnixMilli())
	response, body := browserSessionRequest(t, srv, http.MethodGet, "/v1/users/me", "", token, "", false)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unmarked cookie status=%d body=%s", response.StatusCode, body)
	}
	response, body = browserSessionRequest(t, srv, http.MethodGet, "/v1/users/me", "", token, "", true)
	var view userView
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &view) != nil || view.ID != user.ID || view.Username != user.Username || view.DisplayName != user.DisplayName || view.Role != user.Role || view.Disabled || view.CreatedAt != 0 || view.LastLoginAt != nil {
		t.Fatalf("marked session /me status=%d view=%+v body=%s", response.StatusCode, view, body)
	}
	response, body = browserSessionRequest(t, srv, http.MethodGet, "/v1/users/me", "", token, "Bearer "+browserSessionTestAPIKey, true)
	if response.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no browser session") {
		t.Fatalf("bearer must take priority over browser cookie: status=%d body=%s", response.StatusCode, body)
	}
	response, body = browserSessionRequest(t, srv, http.MethodGet, "/v1/users/me", "", "malformed", "", true)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("malformed session status=%d body=%s", response.StatusCode, body)
	}
	expired := mintBrowserSessionToken(browserSessionTestAPIKey, *user, time.Now().Add(-time.Second).UnixMilli())
	response, body = browserSessionRequest(t, srv, http.MethodGet, "/v1/users/me", "", expired, "", true)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session status=%d body=%s", response.StatusCode, body)
	}
}

func TestBrowserSessionPasswordChangeUsesSessionEpochFence(t *testing.T) {
	store, srv, user := browserSessionTestFixture(t)
	token := mintBrowserSessionToken(browserSessionTestAPIKey, *user, time.Now().Add(time.Hour).UnixMilli())
	response, body := browserSessionRequest(t, srv, http.MethodPost, "/v1/users/me/password", `{"current_password":"wrong password","new_password":"synthetic new password"}`, token, "", true)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "current password is incorrect") {
		t.Fatalf("wrong current password status=%d body=%s", response.StatusCode, body)
	}
	response, body = browserSessionRequest(t, srv, http.MethodPost, "/v1/users/me/password", `{"current_password":"synthetic current password","new_password":"synthetic new password"}`, token, "", true)
	if response.StatusCode != http.StatusOK || string(body) != "{\"changed\":true}\n" {
		t.Fatalf("change password status=%d body=%s", response.StatusCode, body)
	}
	updated, err := store.User(context.Background(), user.ID)
	if err != nil || updated == nil || updated.SessionEpoch != user.SessionEpoch+1 || !browserauth.VerifyPassword("synthetic new password", updated.PasswordHash) {
		t.Fatalf("password transition=%+v err=%v", updated, err)
	}
	response, body = browserSessionRequest(t, srv, http.MethodGet, "/v1/users/me", "", token, "", true)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("password change must revoke prior session: status=%d body=%s", response.StatusCode, body)
	}
}

func TestBrowserSessionPasswordChangeRejectsDisableReenableRace(t *testing.T) {
	store, _, user := browserSessionTestFixture(t)
	ctx := context.Background()
	session := *user
	disabled := true
	if _, err := store.UpdateUser(ctx, user.ID, storage.UserUpdate{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	disabled = false
	if _, err := store.UpdateUser(ctx, user.ID, storage.UserUpdate{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	current, err := store.User(ctx, user.ID)
	if err != nil || current == nil || !current.IsActive() || current.SessionEpoch == session.SessionEpoch {
		t.Fatalf("fixture did not revoke the old session: %+v %v", current, err)
	}
	api := newAPI(store, browserSessionTestAPIKey)
	req := httptest.NewRequest(http.MethodPost, "/v1/users/me/password", strings.NewReader(`{"current_password":"synthetic current password","new_password":"synthetic replacement password"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), browserUserContextKey{}, &session))
	res := httptest.NewRecorder()
	api.changeOwnPassword(res, req)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "user credential or active state changed") {
		t.Fatalf("stale admitted session status=%d body=%s", res.Code, res.Body.String())
	}
	unchanged, err := store.User(ctx, user.ID)
	if err != nil || unchanged == nil || unchanged.PasswordHash != current.PasswordHash || unchanged.SessionEpoch != current.SessionEpoch {
		t.Fatalf("stale session changed account: %+v err=%v", unchanged, err)
	}
}

func TestBrowserAuthenticationPreservesSecurePrincipalAndRemovesForgedIdentityHeaders(t *testing.T) {
	store, _, _ := browserSessionTestFixture(t)
	api := newAPI(store, browserSessionTestAPIKey)
	var principal, userHeader, roleHeader string
	var browserPrincipal bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal = authenticatedPrincipal(r.Context())
		browserPrincipal = authenticatedBrowserUser(r.Context()) != nil
		userHeader = r.Header.Get("X-Gateway-User")
		roleHeader = r.Header.Get("X-Gateway-User-Role")
		w.WriteHeader(http.StatusNoContent)
	})
	handler := api.withGatewayAuthentication(next)
	request := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	request.Header.Set("X-Gateway-User", "999:forged:Admin")
	request.Header.Set("X-Gateway-User-Role", "admin")
	ctx := context.WithValue(request.Context(), securePrincipalContextKey{}, &securePrincipal{NodeID: "node-7", role: "administrator"})
	ctx = context.WithValue(ctx, authenticatedPrincipalContextKey{}, "node-7")
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || principal != "node-7" || browserPrincipal || userHeader != "" || roleHeader != "" {
		t.Fatalf("secure identity was altered or became a browser principal: status=%d principal=%q browser=%v user=%q role=%q", response.Code, principal, browserPrincipal, userHeader, roleHeader)
	}
}
