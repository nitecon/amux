package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClientReloadPublishesIndependentSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	initial := Client{GatewayEndpoint: "https://gateway.example", APIKeyEnv: "GATEWAY_API_KEY",
		Providers: []string{"codex"}, ExcludedProjects: map[string]bool{"example": true}}
	if err := SaveClient(path, initial); err != nil {
		t.Fatal(err)
	}
	if err := InitConfig(path); err != nil {
		t.Fatal(err)
	}
	snapshot := Get()
	snapshot.Client.Providers[0] = "claude"
	delete(snapshot.Client.ExcludedProjects, "example")
	if live := Get().Client; live.Providers[0] != "codex" || !live.ExcludedProjects["example"] {
		t.Fatal("caller modified the active configuration")
	}
	if err := os.WriteFile(path, []byte("unknown-key: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitConfig(path); err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if live := Get().Client; live.Providers[0] != "codex" {
		t.Fatal("invalid reload replaced the active configuration")
	}
	initial.Providers = []string{"claude"}
	if err := SaveClient(path, initial); err != nil {
		t.Fatal(err)
	}
	if live := Get().Client; live.Providers[0] != "codex" {
		t.Fatal("file save bypassed runtime acceptance")
	}
	if err := InitConfig(path); err != nil {
		t.Fatal(err)
	}
	if live := Get().Client; live.Providers[0] != "claude" {
		t.Fatal("valid reload was not published")
	}
}
