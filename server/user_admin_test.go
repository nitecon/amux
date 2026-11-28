package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nitecon/amux/auth"
	account "github.com/nitecon/amux/identity/user"
	"github.com/nitecon/amux/storage"
)

func TestUserAdminRoutesBearerAndAdministratorNodeContracts(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	admin, err := store.CreateUser(ctx, "admin-user", "Admin Display", account.RoleAdmin, "$argon2id$existing")
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.CreateUser(ctx, "member-user", "Member Display", account.RoleMember, "$argon2id$member")
	if err != nil {
		t.Fatal(err)
	}

	a := newAPI(store, "route-test-key")
	mux := http.NewServeMux()
	a.registerUserAdminRoutes(mux)
	requestWithBrowserUser := func(method, path, body string, identity string, role string, browserUser *account.Account) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		ctx := r.Context()
		if identity != "" {
			ctx = auth.WithPrincipal(ctx, identity)
			if role != "" {
				ctx = context.WithValue(ctx, securePrincipalContextKey{}, &securePrincipal{NodeID: identity, role: role})
			}
		}
		if browserUser != nil {
			ctx = auth.WithBrowserUser(ctx, browserUser)
		}
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	request := func(method, path, body string, identity string, role string) *httptest.ResponseRecorder {
		return requestWithBrowserUser(method, path, body, identity, role, nil)
	}

	if response := request(http.MethodGet, "/v1/users", "", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status=%d body=%s", response.Code, response.Body)
	}
	if response := request(http.MethodGet, "/v1/users", "", "node-member", "client"); response.Code != http.StatusForbidden {
		t.Fatalf("member node list status=%d body=%s", response.Code, response.Body)
	}
	if response := requestWithBrowserUser(http.MethodGet, "/v1/users", "", "", "", member); response.Code != http.StatusForbidden {
		t.Fatalf("member session list status=%d body=%s", response.Code, response.Body)
	}
	for _, identity := range []struct{ principal, role string }{
		{auth.BearerPrincipal, ""}, {"node-admin", "administrator"},
	} {
		response := request(http.MethodGet, "/v1/users", "", identity.principal, identity.role)
		if response.Code != http.StatusOK {
			t.Fatalf("administrator list principal=%q role=%q status=%d body=%s", identity.principal, identity.role, response.Code, response.Body)
		}
		var views []map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &views); err != nil || len(views) != 2 {
			t.Fatalf("user list=%s err=%v", response.Body, err)
		}
		if _, ok := views[0]["password_hash"]; ok {
			t.Fatalf("user list leaked password hash: %s", response.Body)
		}
		if _, ok := views[0]["session_epoch"]; ok {
			t.Fatalf("user list leaked session epoch: %s", response.Body)
		}
	}

	createdResponse := request(http.MethodPost, "/v1/users", `{"username":"new-account","password":"eight chars","display_name":"New Display"}`, auth.BearerPrincipal, "")
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body)
	}
	var created userView
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Username != "new-account" || created.Role != account.RoleMember || created.DisplayName != "New Display" {
		t.Fatalf("create response=%s", createdResponse.Body)
	}
	if strings.Contains(createdResponse.Body.String(), `"password_hash"`) {
		t.Fatalf("create response leaked password hash: %s", createdResponse.Body)
	}
	if duplicate := request(http.MethodPost, "/v1/users", `{"username":"NEW-ACCOUNT","password":"eight chars"}`, auth.BearerPrincipal, ""); duplicate.Code != http.StatusBadRequest {
		t.Fatalf("duplicate create status=%d body=%s", duplicate.Code, duplicate.Body)
	}

	oldHash, oldEpoch := member.PasswordHash, member.SessionEpoch
	path := "/v1/users/" + strconv.FormatInt(member.ID, 10)
	updated := request(http.MethodPatch, path, `{"password":"another password","disabled":"true"}`, auth.BearerPrincipal, "")
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body)
	}
	var view map[string]any
	if err := json.Unmarshal(updated.Body.Bytes(), &view); err != nil || view["disabled"] != true {
		t.Fatalf("updated view=%s err=%v", updated.Body, err)
	}
	if _, ok := view["password_hash"]; ok {
		t.Fatalf("update response leaked password hash: %s", updated.Body)
	}
	after, err := store.User(ctx, member.ID)
	if err != nil || after == nil || after.SessionEpoch != oldEpoch+2 || after.PasswordHash == oldHash || after.IsActive() {
		t.Fatalf("updated account=%+v err=%v", after, err)
	}
	if changed, err := store.ChangeUserPassword(ctx, member.ID, oldEpoch, oldHash, "$argon2id$stale"); err != nil || changed {
		t.Fatalf("revoked account session CAS changed=%v err=%v", changed, err)
	}

	adminPath := "/v1/users/" + strconv.FormatInt(admin.ID, 10)
	for _, test := range []struct{ method, body string }{
		{http.MethodPatch, `{"role":"member"}`},
		{http.MethodPatch, `{"disabled":true}`},
		{http.MethodDelete, ""},
	} {
		if response := request(test.method, adminPath, test.body, auth.BearerPrincipal, ""); response.Code != http.StatusBadRequest {
			t.Errorf("last administrator protection %s body=%s status=%d response=%s", test.method, test.body, response.Code, response.Body)
		}
		if response := requestWithBrowserUser(test.method, adminPath, test.body, "", "", admin); response.Code != http.StatusBadRequest {
			t.Errorf("self-protection %s body=%s status=%d response=%s", test.method, test.body, response.Code, response.Body)
		}
	}

	for _, memberPath := range []string{path, "/v1/users/" + strconv.FormatInt(created.ID, 10)} {
		response := request(http.MethodDelete, memberPath, "", auth.BearerPrincipal, "")
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"changed":true}` {
			t.Fatalf("member delete %s status=%d body=%s", memberPath, response.Code, response.Body)
		}
	}
	response := request(http.MethodGet, "/v1/users", "", auth.BearerPrincipal, "")
	var remaining []userView
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &remaining) != nil || len(remaining) != 1 || remaining[0].ID != admin.ID {
		t.Fatalf("final user list status=%d body=%s", response.Code, response.Body)
	}
}

func TestUserAdminRoutesAreRegisteredBehindGatewayAuthentication(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "registered-users.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser(context.Background(), "registered", "", account.RoleMember, "$argon2id$fixture"); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "registered-route-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		authorization string
		want          int
	}{
		{name: "missing bearer", want: http.StatusUnauthorized},
		{name: "bearer admin", authorization: "Bearer registered-route-key", want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
			request.Header.Set("Authorization", test.authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body)
			}
		})
	}
}

func TestUserDisabledValueMatchesRustInputCoercion(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want bool
	}{
		{`true`, true}, {`" on "`, true}, {`"YES"`, true}, {`1`, true}, {`-2`, true},
		{`false`, false}, {`"truthy"`, false}, {`0`, false}, {`1.5`, false}, {`null`, false}, {`[]`, false},
	} {
		if got := userDisabledValue([]byte(test.raw)); got != test.want {
			t.Errorf("userDisabledValue(%s)=%v want=%v", test.raw, got, test.want)
		}
	}
}
