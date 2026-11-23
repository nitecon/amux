package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestArtifactScopedAuthorizationChecksGatewayProjectAndScope(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "artifact-auth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewHandlerWithConfig(store, "fixture-key", HandlerConfig{ArtifactAuthEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		project string
		scopes  string
		want    int
	}{
		{"", "*", http.StatusForbidden},
		{"other", "artifact.read", http.StatusForbidden},
		{"artifact-test", "artifact.write", http.StatusForbidden},
		{" ARTIFACT-TEST ", "artifact.write, artifact.read", http.StatusOK},
		{"artifact-test", "artifact.*", http.StatusOK},
		{"artifact-test", "*", http.StatusOK},
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/projects/artifact-test/artifacts", nil)
		request.Header.Set("Authorization", "Bearer fixture-key")
		request.Header.Set("X-Agent-Project", test.project)
		request.Header.Set("X-Agent-Scopes", test.scopes)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("project %q scopes %q status=%d want=%d: %s", test.project, test.scopes, response.Code, test.want, response.Body)
		}
	}
}

func TestArtifactQuotaOverrideAddsAdminAuthorizationGateOnlyWhenTruthy(t *testing.T) {
	for _, value := range []string{"1", " true ", "YeS"} {
		t.Run(value, func(t *testing.T) {
			a := &api{artifactAuthEnforced: true}
			request := httptest.NewRequest(http.MethodPost, "/v1/projects/demo/artifacts", nil)
			request.SetPathValue("ident", "demo")
			request.Header.Set("X-Agent-Project", "demo")
			request.Header.Set("X-Agent-Scopes", "artifact.write")
			request.Header.Set("X-Artifact-Quota-Override", value)
			response := httptest.NewRecorder()
			if _, ok := a.authorizeArtifactMutation(response, request, "artifact.write"); ok || response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "project.administer") {
				t.Fatalf("override without administer scope: ok=%v status=%d body=%s", ok, response.Code, response.Body.String())
			}
			request.Header.Set("X-Agent-Scopes", "artifact.write project.administer")
			request = withArtifactMutationIdentity(request)
			response = httptest.NewRecorder()
			decision, ok := a.authorizeArtifactMutation(response, request, "artifact.write")
			if !ok || response.Code != http.StatusOK || len(decision.RequiredScopes) != 1 || decision.RequiredScopes[0] != "artifact.write" {
				t.Fatalf("admin override gate changed the operation authorization: decision=%+v ok=%v status=%d", decision, ok, response.Code)
			}
		})
	}
	for _, value := range []string{"", "0", "no", "on", "truthy"} {
		t.Run("not-requested-"+strings.ReplaceAll(value, " ", "blank"), func(t *testing.T) {
			a := &api{artifactAuthEnforced: true}
			request := httptest.NewRequest(http.MethodPost, "/v1/projects/demo/artifacts", nil)
			request.SetPathValue("ident", "demo")
			request.Header.Set("X-Agent-Project", "demo")
			request.Header.Set("X-Agent-Scopes", "artifact.write")
			request.Header.Set("X-Artifact-Quota-Override", value)
			request = withArtifactMutationIdentity(request)
			response := httptest.NewRecorder()
			decision, ok := a.authorizeArtifactMutation(response, request, "artifact.write")
			if !ok || response.Code != http.StatusOK || len(decision.RequiredScopes) != 1 {
				t.Fatalf("false override unexpectedly required admin: decision=%+v ok=%v status=%d", decision, ok, response.Code)
			}
		})
	}
}

func withArtifactMutationIdentity(request *http.Request) *http.Request {
	request.Header.Set("Idempotency-Key", "quota-test")
	request.Header.Set("X-Agent-Id", "agent-1")
	request.Header.Set("X-Agent-System", "codex")
	ctx := context.WithValue(request.Context(), authenticatedPrincipalContextKey{}, bearerAPIKeyPrincipal)
	return request.WithContext(ctx)
}
