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

func TestProjectDeleteRequiresConfirmationAndSuppressesAutomaticRecreation(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "internal", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		method, path, body, token string
		want                      int
	}{
		{http.MethodDelete, "/v1/projects/demo", `{"confirmation":"demo"}`, "", 401},
		{http.MethodDelete, "/v1/projects/demo", `{"confirmation":"DEMO"}`, "fixture-key", 400},
		{http.MethodDelete, "/v1/projects/demo", `{"confirmation":"demo"}`, "fixture-key", 204},
		{http.MethodPost, "/v1/projects", `{"ident":"demo","repo_url":"github.com/example/demo"}`, "fixture-key", 410},
		{http.MethodPost, "/v1/projects", `{"ident":"demo","repo_url":"github.com/example/demo","recreate":true}`, "fixture-key", 200},
	} {
		request := httptest.NewRequest(attempt.method, attempt.path, strings.NewReader(attempt.body))
		if attempt.token != "" {
			request.Header.Set("Authorization", "Bearer "+attempt.token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != attempt.want {
			t.Fatalf("%s %s returned %d, expected %d: %s", attempt.method, attempt.path, response.Code, attempt.want, response.Body.String())
		}
	}
}
