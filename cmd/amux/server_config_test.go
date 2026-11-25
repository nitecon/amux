package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/gatewayclient"
)

func TestGenerateAndRunServerConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--generate-server-config"}, &output, io.Discard, profileTestEnv); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(serverConfigFilename)
	if err != nil || !strings.Contains(output.String(), filepath.Join(dir, serverConfigFilename)) ||
		!bytes.Contains(data, []byte("profile-tls-name: []")) || !bytes.Contains(data, []byte("message-retention-days: 30")) {
		t.Fatalf("generated file %q, output %q, error %v", data, output.String(), err)
	}
	if err := run(context.Background(), []string{"--generate-server-config"}, io.Discard, io.Discard, profileTestEnv); err == nil {
		t.Fatal("regeneration should reject an existing file")
	}
	data = bytes.Replace(data, []byte("database: \"\""), []byte("database: application.sqlite"), 1)
	if err := os.WriteFile(serverConfigFilename, data, 0600); err != nil {
		t.Fatal(err)
	}
	address, stop := startServerCommand(t, "--config", serverConfigFilename, "--listen", "127.0.0.1:0")
	defer stop()
	client, err := gatewayclient.New("http://"+address, profileTestEnv("GATEWAY_API_KEY"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Projects(context.Background()); err != nil {
		t.Fatalf("configured gateway request: %v", err)
	}
	if err := os.WriteFile("bad.yml", []byte("unknown-setting: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"server", "run", "--config", "bad.yml"}, io.Discard, io.Discard, profileTestEnv); err == nil || !strings.Contains(err.Error(), "unknown server setting") {
		t.Fatalf("invalid setting returned %v", err)
	}
}
