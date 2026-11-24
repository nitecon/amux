package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/gatewayclient"
)

func TestBrowserNavigationStreamRefreshesReconnectsAndCancels(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	var projectRequests atomic.Int32
	var streamConnections atomic.Int32
	change := make(chan struct{}, 1)
	restore := make(chan struct{})
	connected := make(chan int, 4)
	streamCanceled := make(chan struct{}, 2)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects":
			projectRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]any{})
		case "/v1/navigation/stream":
			if r.Header.Get("Authorization") != "Bearer browser-test" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			index := int(streamConnections.Add(1))
			if index == 2 {
				select {
				case <-restore:
				case <-r.Context().Done():
					return
				}
			}
			if wsjson.Write(r.Context(), conn, map[string]any{"version": 1, "type": "ready"}) != nil {
				return
			}
			connected <- index
			if index == 1 {
				select {
				case <-change:
					_ = wsjson.Write(r.Context(), conn, map[string]any{"version": 1, "type": "changed"})
					return // Drop the wire to make the browser reconnect.
				case <-r.Context().Done():
					streamCanceled <- struct{}{}
					return
				}
			}
			<-conn.CloseRead(r.Context()).Done()
			streamCanceled <- struct{}{}
		default:
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "browser-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	browser.Start(context.Background())
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })

	waitBrowser(t, callbacks, func() bool { return projectRequests.Load() >= 1 })
	select {
	case index := <-connected:
		if index != 1 {
			t.Fatalf("first stream connection index = %d", index)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("browser did not connect to navigation stream")
	}
	change <- struct{}{}
	waitBrowser(t, callbacks, func() bool {
		return browser.navigationReconnecting && strings.Contains(browser.headerGateway.Text, "reconnecting")
	})
	close(restore)
	waitBrowser(t, callbacks, func() bool { return projectRequests.Load() >= 2 })
	select {
	case index := <-connected:
		if index != 2 {
			t.Fatalf("reconnected stream index = %d", index)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("browser did not reconnect after the dropped stream")
	}
	// Ready on the replacement connection forces a full reload. It coalesces
	// with the preceding change when both arrive inside the one-second window.
	waitBrowser(t, callbacks, func() bool { return projectRequests.Load() >= 2 && !browser.navigationReconnecting })

	browser.Close()
	drained := make(chan struct{})
	go func() { browser.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not join the navigation stream and refresh worker")
	}
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the browser did not cancel the active stream")
	}
}

func TestBrowserPollsProjectsWhenGatewayHasNoNavigationStream(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	var projectRequests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer browser-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/projects":
			projectRequests.Add(1)
			_, _ = w.Write([]byte("[]"))
		case "/v1/presence/summary":
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "browser-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	browser.Start(context.Background())
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })
	waitBrowser(t, callbacks, func() bool {
		return projectRequests.Load() >= 2 && strings.Contains(browser.headerGateway.Text, "Gateway: connected")
	})
}
