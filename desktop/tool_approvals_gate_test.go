package desktop

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func TestAlwaysAllowOnlyForConversationsOnThisHostsWorker(t *testing.T) {
	host := httptest.NewServer(http.NotFoundHandler())
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
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })
	browser.workPreferencesPath = filepath.Join(t.TempDir(), "work-preferences.json")
	local, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	local = strings.TrimSpace(local)
	remote := "remote-" + local
	for name, test := range map[string]struct {
		hostname *string
		want     string
	}{
		"local worker":  {&local, browser.workPreferencesPath},
		"remote worker": {&remote, ""},
		"local chat":    {nil, ""},
	} {
		chat := browser.chatForConversation(storage.Conversation{ID: name, ProjectIdent: "demo", Provider: "claude", FleetHostname: test.hostname})
		if chat == nil {
			t.Fatalf("%s: conversation did not open", name)
		}
		if chat.toolApprovalsPath != test.want {
			t.Errorf("%s: tool approvals path = %q, want %q", name, chat.toolApprovalsPath, test.want)
		}
	}
}
