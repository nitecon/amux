package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestWorkerPresencePrincipalComesFromServerAuthentication(t *testing.T) {
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "presence-principal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	spoofed := httptest.NewRequest("POST", "/v1/worker-presence", strings.NewReader(`{"source":"api-source","sequence":1,"principal":"spoofed","agents":[]}`))
	spoofed.Header.Set("Authorization", "Bearer key")
	spoofed.Header.Set("X-Authenticated-Principal", "header-principal")
	spoofedResult := httptest.NewRecorder()
	h.ServeHTTP(spoofedResult, spoofed)
	if spoofedResult.Code != 400 {
		t.Fatalf("body principal accepted: status=%d body=%s", spoofedResult.Code, spoofedResult.Body.String())
	}
	for i, headerPrincipal := range []string{"first-untrusted-header", "second-untrusted-header"} {
		sequence := []string{"1", "2"}[i]
		body := `{"source":"api-source","sequence":` + sequence + `,"agents":[]}`
		req := httptest.NewRequest("POST", "/v1/worker-presence", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer key")
		req.Header.Set("X-Authenticated-Principal", headerPrincipal)
		req.Header.Set("X-Gateway-User", headerPrincipal)
		req.Header.Set("X-Gateway-User-Role", "admin")
		result := httptest.NewRecorder()
		h.ServeHTTP(result, req)
		if result.Code != 200 {
			t.Fatalf("server principal changed with untrusted header %q: status=%d body=%s", headerPrincipal, result.Code, result.Body.String())
		}
	}
}
