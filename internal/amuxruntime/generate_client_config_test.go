package amuxruntime

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/config"
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
	path := filepath.Join(home, ".amux", "config.yaml")
	settings, err := config.ReadClient(path)
	if err != nil || settings.GatewayEndpoint != "https://gateway.example" || settings.APIKeyEnv != "GATEWAY_API_KEY" || settings.ProjectsDirectory != home || !strings.Contains(output.String(), path) {
		t.Fatalf("generated settings %+v, output %q, error %v", settings, output.String(), err)
	}
	before, err := os.ReadFile(path)
	if err != nil || bytes.Contains(before, []byte("projects_directory")) {
		t.Fatalf("bootstrap included device settings: %s (%v)", before, err)
	}
	device, err := os.ReadFile(filepath.Join(home, ".amux", "device.yaml"))
	if err != nil || !bytes.Contains(device, []byte("projects_directory")) {
		t.Fatalf("missing device settings: %s (%v)", device, err)
	}
	if err := run(context.Background(), []string{"--generate-client-config"}, io.Discard, io.Discard, os.Getenv); err == nil {
		t.Fatal("generation overwrote an existing configuration")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing configuration changed: %v", err)
	}
}
