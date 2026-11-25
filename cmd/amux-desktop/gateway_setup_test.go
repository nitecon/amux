//go:build desktop

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestEndpointIsThisHost(t *testing.T) {
	ctx := context.Background()
	name, err := os.Hostname()
	if err != nil {
		t.Skip("hostname unavailable")
	}
	for _, endpoint := range []string{"https://127.0.0.1:7913", "https://localhost:7913", "https://[::1]:7913", "https://" + name + ":7913"} {
		if !endpointIsThisHost(ctx, endpoint) {
			t.Fatalf("%s should be this host", endpoint)
		}
	}
	label := strings.SplitN(name, ".", 2)[0]
	for _, endpoint := range []string{"", "https://192.0.2.10:7913", "not a url", "https://" + label + ".remote.invalid:7913"} {
		if endpointIsThisHost(ctx, endpoint) {
			t.Fatalf("%q should not be this host", endpoint)
		}
	}
}
