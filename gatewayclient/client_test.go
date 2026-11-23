package gatewayclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayClientRejectsUnsafeOriginsAndRedirects(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "https://user:password@example.com", "https://example.com/path", "https://example.com/?token=secret", "https://example.com/#fragment", "file:///tmp/state"} {
		if _, err := New(endpoint, "secret", nil); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	if _, err := New("https://example.com/", "secret", nil); err != nil {
		t.Fatalf("root path rejected: %v", err)
	}
	if _, err := New("http://127.0.0.1:7913", "secret\r\nInjected: header", nil); err == nil {
		t.Fatal("invalid credential accepted")
	}
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := New(source.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Conversation(context.Background(), "demo", "id")
	var response *HTTPError
	if !errors.As(err, &response) || response.Status != 307 || forwarded.Load() != 0 {
		t.Fatalf("redirect followed: %v; forwarded=%d", err, forwarded.Load())
	}
}
func TestGatewayErrorsExcludeResponsePayloadAndCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("secret-key and private tool payload"))
	}))
	defer srv.Close()
	client, err := New(srv.URL, "secret-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Conversation(context.Background(), "demo", "id")
	if err == nil || !Retryable(err) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "payload") {
		t.Fatalf("unsafe error: %v", err)
	}
}
