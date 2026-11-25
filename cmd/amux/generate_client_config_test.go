package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/internal/clientconfig"
)

func TestGenerateClientConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var output bytes.Buffer
	args := []string{"--generate-client-config", "--server", "https://gateway.example", "--api-key-env", "GATEWAY_API_KEY"}
	if err := run(context.Background(), args, &output, io.Discard, os.Getenv); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".amux", "config.yml")
	config, err := clientconfig.Load(path)
	if err != nil || config.GatewayEndpoint != "https://gateway.example" || config.APIKeyEnv != "GATEWAY_API_KEY" || config.ProjectsDirectory != home || !strings.Contains(output.String(), path) {
		t.Fatalf("generated config %+v, output %q, error %v", config, output.String(), err)
	}
	before, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(before, []byte("project_urls: []")) {
		t.Fatalf("missing editable URL list: %v", err)
	}
	if err := run(context.Background(), []string{"--generate-client-config"}, io.Discard, io.Discard, os.Getenv); err == nil {
		t.Fatal("generation overwrote an existing config")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing configuration changed: %v", err)
	}
}
