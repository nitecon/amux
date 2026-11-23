//go:build desktop

package main

import (
	"path/filepath"
	"testing"
)

func TestGatewayStatePathIsolatesExplicitPreferences(t *testing.T) {
	config := t.TempDir()
	preferences := filepath.Join(t.TempDir(), "gateway.json")
	got, err := gatewayStatePath(config, preferences, "", true)
	want, _ := filepath.Abs(filepath.Join(filepath.Dir(preferences), "gateway-profile"))
	if err != nil || got != want {
		t.Fatalf("explicit preferences state path=%q want=%q err=%v", got, want, err)
	}
	got, err = gatewayStatePath(config, preferences, "", false)
	want, _ = filepath.Abs(filepath.Join(config, "amux"))
	if err != nil || got != want {
		t.Fatalf("default state path=%q want=%q err=%v", got, want, err)
	}
	explicit := filepath.Join(t.TempDir(), "override")
	got, err = gatewayStatePath(config, preferences, explicit, true)
	want, _ = filepath.Abs(explicit)
	if err != nil || got != want {
		t.Fatalf("explicit state dir=%q want=%q err=%v", got, want, err)
	}
}
