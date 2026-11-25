package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientControlReloadReportsRejectedConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := filepath.Join(t.TempDir(), "config.yml")
	reloads := make(chan clientReload)
	stop, err := serveClientControl(config, "https://gateway.test", "test-build", reloads, ctx.Done(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	address, err := os.ReadFile(config + ".control")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post("http://"+strings.Fields(string(address))[0]+"/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reload returned %d", response.StatusCode)
	}
	go func() {
		for _, result := range []string{"Applied one project binding.", "Rejected configuration: invalid YAML"} {
			select {
			case request := <-reloads:
				request.reply <- result
			case <-ctx.Done():
				return
			}
		}
	}()
	var stdout bytes.Buffer
	if err := requestClientReloadAt(ctx, config, &stdout); err != nil || !strings.Contains(stdout.String(), "Applied one project binding") {
		t.Fatalf("reload returned %q, %v", stdout.String(), err)
	}
	if err := requestClientReloadAt(ctx, config, &stdout); err == nil || !strings.Contains(err.Error(), "invalid YAML") {
		t.Fatalf("invalid reload returned %v", err)
	}
}
