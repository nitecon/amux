package desktop

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"github.com/google/uuid"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestGlobalNotificationConsumerPersistsPerGateway(t *testing.T) {
	path := filepath.Join(t.TempDir(), "navigation.json")
	makeBrowser := func(url string) *Browser {
		t.Helper()
		client, err := gatewayclient.New(url, "test-key", nil)
		if err != nil {
			t.Fatal(err)
		}
		prefs, err := LoadNavigationPreferences(path)
		if err != nil {
			t.Fatal(err)
		}
		b := &Browser{client: client, prefs: prefs, prefsPath: path}
		b.configureGlobalNotifications(func(string) {})
		if b.notificationConsumer == "" {
			t.Fatal("consumer was not created")
		}
		return b
	}
	first := makeBrowser("https://one.example").notificationConsumer
	second := makeBrowser("https://two.example").notificationConsumer
	restarted := makeBrowser("https://one.example").notificationConsumer
	if first != restarted || first == second {
		t.Fatalf("consumers first=%q other=%q restarted=%q", first, second, restarted)
	}
}

func TestBrowserHandsGlobalNoticesToFyneInOrder(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "notifications.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	builtins, err := server.NewGatewayFISM(store, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandlerWithRemote(store, "desktop-fixture", server.RemoteConfig{
		Context: ctx, Registry: builtins.Registry(), Forwarded: controllers.RemoteForwardedEvents(),
	})
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(cleanup)
	}()
	client, err := gatewayclient.New(host.URL, "desktop-fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	queue := newUICallbacks()
	var messages []string
	browser, err := NewBrowser(BrowserConfig{
		Client: client, Dispatch: queue.dispatch, PreferencesPath: filepath.Join(t.TempDir(), "navigation.json"),
		NotifyGlobal: func(message string) {
			app.SendNotification(fyne.NewNotification("Amux notification", message))
			messages = append(messages, message)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { browser.Close(); browser.Wait(); queue.pump() }()
	after := int64(0)
	if _, err := store.OpenRemoteConsumer(ctx, browser.notificationConsumer, uuid.NewString(), &after); err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"First", "Second"} {
		payload, err := json.Marshal(map[string]string{"message": message})
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.EmitRouterEvent(ctx, builtins.Registry(), fism.EmitRequest{
			ID: controllers.GlobalNotifyEventID, Version: 1, Source: "fixture", Payload: payload,
		})
		if err != nil || !result.Stored {
			t.Fatalf("publish %q: %+v %v", message, result, err)
		}
	}
	browser.Start(ctx)
	fynetest.AssertNotificationSent(t, fyne.NewNotification("Amux notification", "Second"), func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for len(messages) < 2 {
			queue.pump()
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatalf("notifications delivered: %v", messages)
			}
		}
	})
	if !slices.Equal(messages, []string{"First", "Second"}) {
		t.Fatalf("notifications delivered out of order: %v", messages)
	}
}
