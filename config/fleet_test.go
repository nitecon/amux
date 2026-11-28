package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/gatewayclient"
)

func TestFleetSettingsIntersectDeviceProvidersAndRejectFailedReload(t *testing.T) {
	var failed atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/providers" || r.Header.Get("Authorization") != "Bearer fixture" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if failed.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `[{"id":"antigravity","enabled":true},{"id":"claude","enabled":false},{"id":"codex","enabled":true}]`)
	}))
	defer host.Close()
	gateway, err := gatewayclient.New(host.URL, "fixture", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	settings := Client{GatewayEndpoint: host.URL, APIKeyEnv: "GATEWAY_API_KEY", Providers: []string{"claude", "codex"}}
	if err := SaveClient(path, settings); err != nil {
		t.Fatal(err)
	}
	if err := LoadClientFleet(context.Background(), path, gateway); err != nil {
		t.Fatal(err)
	}
	if got := Get().Providers; !slices.Equal(got, []string{"codex"}) {
		t.Fatalf("effective fleet providers = %v", got)
	}
	failed.Store(true)
	if err := LoadClientFleet(context.Background(), path, gateway); err == nil {
		t.Fatal("accepted a failed fleet settings request")
	}
	if got := Get().Providers; !slices.Equal(got, []string{"codex"}) {
		t.Fatalf("failed reload changed providers to %v", got)
	}
}
