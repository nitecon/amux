package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClientReloadPublishesValidatedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	initial := Client{GatewayEndpoint: "https://gateway.example", APIKeyEnv: "GATEWAY_API_KEY",
		Providers: []string{"codex"}, ExcludedProjects: map[string]bool{"example": true}}
	if err := SaveClient(path, initial); err != nil {
		t.Fatal(err)
	}
	if err := LoadClient(path); err != nil {
		t.Fatal(err)
	}
	previous := Get()
	if err := os.WriteFile(path, []byte("unknown-key: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadClient(path); err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if live := Get(); live.Providers[0] != "codex" {
		t.Fatal("invalid reload replaced the active configuration")
	}
	initial.Providers = []string{"claude"}
	if err := SaveClient(path, initial); err != nil {
		t.Fatal(err)
	}
	if live := Get(); live.Providers[0] != "codex" {
		t.Fatal("file save bypassed runtime acceptance")
	}
	rejected := errors.New("restart required")
	if err := LoadClient(path, func(Client) error { return rejected }); !errors.Is(err, rejected) {
		t.Fatalf("load returned %v, want %v", err, rejected)
	}
	if live := Get(); live.Providers[0] != "codex" {
		t.Fatal("rejected reload replaced the active configuration")
	}
	if err := LoadClient(path); err != nil {
		t.Fatal(err)
	}
	if live := Get(); live.Providers[0] != "claude" {
		t.Fatal("valid reload was not published")
	}
	if previous.Providers[0] != "codex" || !previous.ExcludedProjects["example"] {
		t.Fatal("reload mutated previously published settings")
	}
}
