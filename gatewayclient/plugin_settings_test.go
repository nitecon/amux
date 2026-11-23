package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestPluginSettingsRealServerListAndUpdate(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, err := New(host.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}

	settings, err := client.PluginSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := storage.SwitchablePluginIDs()
	if len(settings) != len(ids) {
		t.Fatalf("settings count=%d, want %d", len(settings), len(ids))
	}
	for i, id := range ids {
		if settings[i] != (storage.PluginSetting{ID: id, Enabled: true}) {
			t.Fatalf("setting[%d]=%+v, want enabled %s", i, settings[i], id)
		}
	}

	updated, err := client.SetPluginEnabled(ctx, "review", false)
	if err != nil || updated == nil || *updated != (storage.PluginSetting{ID: "review", Enabled: false}) {
		t.Fatalf("disabled receipt: %+v %v", updated, err)
	}
	settings, err = client.PluginSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range settings {
		if setting.ID == "review" && setting.Enabled {
			t.Fatal("review plugin remained enabled after successful update")
		}
	}
}

func TestPluginSettingsRejectsMalformedListAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"incomplete", `[{"id":"assignment","enabled":true}]`},
		{"wrong order", `[{"id":"notifier","enabled":true},{"id":"assignment","enabled":true},{"id":"reminder","enabled":true},{"id":"review","enabled":true},{"id":"reclaim","enabled":true}]`},
		{"duplicate", `[{"id":"assignment","enabled":true},{"id":"notifier","enabled":true},{"id":"reminder","enabled":true},{"id":"review","enabled":true},{"id":"review","enabled":true}]`},
		{"unknown id", `[{"id":"assignment","enabled":true},{"id":"notifier","enabled":true},{"id":"reminder","enabled":true},{"id":"review","enabled":true},{"id":"remote","enabled":true}]`},
		{"missing enabled", `[{"id":"assignment"},{"id":"notifier","enabled":true},{"id":"reminder","enabled":true},{"id":"review","enabled":true},{"id":"reclaim","enabled":true}]`},
		{"null enabled", `[{"id":"assignment","enabled":null},{"id":"notifier","enabled":true},{"id":"reminder","enabled":true},{"id":"review","enabled":true},{"id":"reclaim","enabled":true}]`},
		{"null list", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			if _, err := client.PluginSettings(context.Background()); err == nil {
				t.Fatal("accepted malformed plugin settings list")
			}
		})
	}
}

func TestSetPluginEnabledRejectsMalformedAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"wrong id", `{"id":"notifier","enabled":false}`},
		{"wrong value", `{"id":"review","enabled":true}`},
		{"missing enabled", `{"id":"review"}`},
		{"null enabled", `{"id":"review","enabled":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			if _, err := client.SetPluginEnabled(context.Background(), "review", false); err == nil {
				t.Fatal("accepted malformed plugin setting acknowledgement")
			}
		})
	}
}

type lostPluginAckTransport struct {
	base   http.RoundTripper
	writes atomic.Int32
}

func (t *lostPluginAckTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if request.Method == http.MethodPut {
		t.writes.Add(1)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil, errors.New("simulated lost acknowledgement")
	}
	return response, nil
}

func TestSetPluginEnabledDoesNotRetryLostAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "lost-ack.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	transport := &lostPluginAckTransport{base: http.DefaultTransport}
	httpClient := &http.Client{Transport: transport}
	client, err := New(host.URL, "secret", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetPluginEnabled(ctx, "review", false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected uncertain transport error, got %v", err)
	}
	if got := transport.writes.Load(); got != 1 {
		t.Fatalf("PUT count=%d, want exactly one", got)
	}
	enabled, err := store.PluginEnabled(ctx, "review")
	if err != nil || enabled {
		t.Fatalf("lost acknowledgement did not follow one persisted write: enabled=%v err=%v", enabled, err)
	}
}

func TestPluginSettingsWireBooleanPresence(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["enabled"]) != "false" {
				t.Errorf("PUT omitted explicit false: %v", body)
			}
			_, _ = w.Write([]byte(`{"id":"review","enabled":false}`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"assignment","enabled":true},{"id":"notifier","enabled":true},{"id":"reminder","enabled":true},{"id":"review","enabled":false},{"id":"reclaim","enabled":true}]`))
	}))
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)
	setting, err := client.SetPluginEnabled(context.Background(), "review", false)
	if err != nil || setting == nil || setting.Enabled {
		t.Fatalf("false setting receipt: %+v %v", setting, err)
	}
}
