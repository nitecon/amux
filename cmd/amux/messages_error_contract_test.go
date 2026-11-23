package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/gatewayclient"
)

func TestMessagesCLIReadFailureIsNotSuccessfulNullOutput(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "synthetic unavailable", http.StatusServiceUnavailable)
	}))
	defer host.Close()
	var stdout, stderr bytes.Buffer
	err := runMessages(context.Background(), []string{"--server", host.URL, "--project", "demo", "list"}, &stdout, &stderr, func(name string) string {
		if name == "GATEWAY_API_KEY" {
			return "synthetic-key"
		}
		return ""
	})
	var responseError *gatewayclient.HTTPError
	if !errors.As(err, &responseError) || responseError.Status != http.StatusServiceUnavailable {
		t.Fatalf("CLI discarded HTTP failure: err=%v stdout=%q", err, stdout.String())
	}
	if stdout.Len() != 0 || requests.Load() != 1 {
		t.Fatalf("failed read emitted success or retried: stdout=%q requests=%d", stdout.String(), requests.Load())
	}
}
