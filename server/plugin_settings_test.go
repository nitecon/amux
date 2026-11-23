package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

const pluginSettingsTestKey = "plugin-settings-test-key"

func pluginSettingsFixture(t *testing.T) (*storage.Store, http.Handler) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(store, pluginSettingsTestKey)
	if err != nil {
		t.Fatal(err)
	}
	return store, handler
}

func pluginSettingsRequest(handler http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestPluginSettingsNewHandlerRequiresBearerAndRejectsForgedGatewayHeaders(t *testing.T) {
	_, handler := pluginSettingsFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		path := "/v1/plugins"
		if method == http.MethodPut {
			path = "/v1/plugins/review"
		}
		response := pluginSettingsRequest(handler, method, path, `{"enabled":false}`, "")
		if response.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s status=%d, want 401", method, response.Code)
		}
	}
	forged := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	forged.Header.Set("X-Gateway-User", "admin")
	forged.Header.Set("X-Gateway-User-Role", "admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, forged)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("forged identity without bearer status=%d, want 401", response.Code)
	}
}

func TestPluginSettingsNewHandlerBearerListsUpdatesAndRejectsNonSwitchable(t *testing.T) {
	store, handler := pluginSettingsFixture(t)
	get := pluginSettingsRequest(handler, http.MethodGet, "/v1/plugins", "", pluginSettingsTestKey)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	var settings []storage.PluginSetting
	if err := json.Unmarshal(get.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	want := []storage.PluginSetting{
		{ID: "assignment", Enabled: true},
		{ID: "notifier", Enabled: true},
		{ID: "reminder", Enabled: true},
		{ID: "review", Enabled: true},
		{ID: "reclaim", Enabled: true},
	}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("GET settings=%+v, want %+v", settings, want)
	}
	for _, unsupported := range []string{"remote", "native-delivery"} {
		for _, setting := range settings {
			if setting.ID == unsupported {
				t.Fatalf("non-switchable plugin %q appeared in API", unsupported)
			}
		}
	}

	put := pluginSettingsRequest(handler, http.MethodPut, "/v1/plugins/review", `{"enabled":false,"ignored":true}`, pluginSettingsTestKey)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body.String())
	}
	var updated storage.PluginSetting
	if err := json.Unmarshal(put.Body.Bytes(), &updated); err != nil || updated != (storage.PluginSetting{ID: "review", Enabled: false}) {
		t.Fatalf("PUT response=%+v err=%v", updated, err)
	}
	enabled, err := store.PluginEnabled(context.Background(), "review")
	if err != nil || enabled {
		t.Fatalf("persisted review enabled=%v err=%v", enabled, err)
	}
	put = pluginSettingsRequest(handler, http.MethodPut, "/v1/plugins/review", `{"enabled":true}`, pluginSettingsTestKey)
	if put.Code != http.StatusOK {
		t.Fatalf("re-enable status=%d body=%s", put.Code, put.Body.String())
	}
	enabled, err = store.PluginEnabled(context.Background(), "review")
	if err != nil || !enabled {
		t.Fatalf("persisted review enabled=%v err=%v", enabled, err)
	}
	for _, id := range []string{"remote", "native-delivery"} {
		response := pluginSettingsRequest(handler, http.MethodPut, "/v1/plugins/"+id, `{"enabled":false}`, pluginSettingsTestKey)
		if response.Code != http.StatusNotFound {
			t.Errorf("non-switchable plugin %q status=%d, want 404", id, response.Code)
		}
	}
}

func TestPluginSettingsNewHandlerRejectsBadBodyAndUnknownPlugin(t *testing.T) {
	store, handler := pluginSettingsFixture(t)
	for _, body := range []string{`{}`, `null`, `{"enabled":null}`, `{"enabled":"false"}`, `{"enabled":0}`, `{`, `{"enabled":false} {}`} {
		response := pluginSettingsRequest(handler, http.MethodPut, "/v1/plugins/review", body, pluginSettingsTestKey)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %s status=%d, want 400 (%s)", body, response.Code, response.Body.String())
		}
	}
	if enabled, err := store.PluginEnabled(context.Background(), "review"); err != nil || !enabled {
		t.Fatalf("invalid requests changed setting: enabled=%v err=%v", enabled, err)
	}
	for _, id := range []string{"unknown", "remote", "native-delivery"} {
		response := pluginSettingsRequest(handler, http.MethodPut, "/v1/plugins/"+id, `{"enabled":false}`, pluginSettingsTestKey)
		if response.Code != http.StatusNotFound {
			t.Errorf("plugin %q status=%d, want 404 (%s)", id, response.Code, response.Body.String())
		}
	}
}

func TestPluginSettingsNewHandlerUnknownIDPrecedesEnabledValidation(t *testing.T) {
	_, handler := pluginSettingsFixture(t)
	for _, body := range []string{`{}`, `{"enabled":null}`} {
		response := pluginSettingsRequest(handler, http.MethodPut, "/v1/plugins/remote", body, pluginSettingsTestKey)
		if response.Code != http.StatusNotFound {
			t.Errorf("valid JSON body %s status=%d, want 404 (%s)", body, response.Code, response.Body.String())
		}
	}
	malformed := pluginSettingsRequest(handler, http.MethodPut, "/v1/plugins/remote", `{`, pluginSettingsTestKey)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON status=%d, want 400", malformed.Code)
	}
}
