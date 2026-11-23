package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayPreferencesPersistModeWithoutPeeringKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.json")
	prefs := GatewayPreferences{
		Mode: GatewayModeClient, Endpoint: "https://gateway.example:7913",
		EnrolledProfile: "/profiles/client-profile.json",
	}
	if err := SaveGatewayPreferences(path, prefs); err != nil {
		t.Fatal(err)
	}
	got, err := LoadGatewayPreferences(path)
	if err != nil || got != prefs {
		t.Fatalf("loaded preferences %+v, err=%v", got, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "amux1.") || strings.Contains(string(data), "invitation") {
		t.Fatalf("gateway preferences persisted invitation material: %s", data)
	}
}

func TestGatewayPreferencesDefaultAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	got, err := LoadGatewayPreferences(path)
	if err != nil || got.Mode != GatewayModeServer {
		t.Fatalf("default preferences %+v, err=%v", got, err)
	}
	if err := SaveGatewayPreferences(path, GatewayPreferences{Mode: GatewayModeClient}); err == nil {
		t.Fatal("accepted client mode without endpoint/profile")
	}
}

func TestGatewayPreferencesRequireExplicitAdvertisedHostForRemoteHosting(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{
		{" gateway.example. ", "gateway.example", true},
		{"192.0.2.12", "192.0.2.12", true},
		{"2001:db8::1", "2001:db8::1", true},
		{"gateway.example:7913", "", false},
		{"https://gateway.example", "", false},
		{"", "", false},
	} {
		got, err := NormalizeAdvertisedHost(tc.input)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("NormalizeAdvertisedHost(%q)=%q err=%v", tc.input, got, err)
		}
	}
	path := filepath.Join(t.TempDir(), "gateway.json")
	prefs := GatewayPreferences{Mode: GatewayModeServer, RemoteHosting: true, AdvertisedHost: "gateway.example"}
	if err := SaveGatewayPreferences(path, prefs); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadGatewayPreferences(path)
	if err != nil || loaded != prefs {
		t.Fatalf("remote preferences=%+v err=%v", loaded, err)
	}
	if err := SaveGatewayPreferences(path, GatewayPreferences{Mode: GatewayModeServer, RemoteHosting: true}); err == nil {
		t.Fatal("remote hosting was saved without an advertised host")
	}
}
