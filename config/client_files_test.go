package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestClientBootstrapKeepsDeviceSettingsInSeparateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	want := Client{
		GatewayEndpoint: "https://gateway.example", APIKeyEnv: "GATEWAY_API_KEY",
		WorkerJournal: filepath.Join(t.TempDir(), "workers.db"), Providers: []string{},
	}
	if err := SaveClient(path, want); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := os.ReadFile(path)
	if err != nil || bytes.Contains(bootstrap, []byte("worker_journal")) || bytes.Contains(bootstrap, []byte("providers")) {
		t.Fatalf("bootstrap contains device settings: %s (%v)", bootstrap, err)
	}
	got, err := ReadClient(path)
	if err != nil || got.GatewayEndpoint != want.GatewayEndpoint || got.WorkerJournal != want.WorkerJournal || got.Providers == nil || len(got.Providers) != 0 {
		t.Fatalf("configuration after round trip = %+v, %v", got, err)
	}
}

func TestClientBootstrapMigratesExistingDeviceSettings(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "config.yml")
	path := filepath.Join(dir, "config.yaml")
	settings := Client{
		GatewayEndpoint: "https://gateway.example", APIKeyEnv: "GATEWAY_API_KEY",
		WorkerJournal: filepath.Join(dir, "workers.db"),
	}
	if err := SaveClient(oldPath, settings); err != nil {
		t.Fatal(err)
	}
	if err := LoadClient(path); err != nil {
		t.Fatal(err)
	}
	got, err := ReadClient(path)
	if err != nil || got.WorkerJournal != settings.WorkerJournal || got.GatewayEndpoint != settings.GatewayEndpoint {
		t.Fatalf("migrated configuration = %+v, %v", got, err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal(err)
	}
}
