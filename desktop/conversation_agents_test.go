package desktop

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"slices"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestConversationAgentsOrderLocalFirstThenByOperatingSystem(t *testing.T) {
	host := "local-host"
	localOS := runtime.GOOS
	if localOS == "darwin" {
		localOS = "macos"
	}
	makeAgent := func(id, platform string, hostname *string) storage.AgentPresence {
		return storage.AgentPresence{PresenceAgent: storage.PresenceAgent{
			Origin: agent.Origin{SessionID: id, OS: platform}, Hostname: hostname,
		}}
	}
	linuxHost, windowsHost := "linux-host", "windows-host"
	agents := []storage.AgentPresence{
		makeAgent("windows", "windows", &windowsHost),
		makeAgent("linux-unknown", "linux", nil),
		makeAgent("linux", "linux", &linuxHost),
		makeAgent("local", localOS, &host),
	}
	sortConversationAgents(agents, host)
	var order []string
	for _, agent := range agents {
		order = append(order, agent.Origin.SessionID)
	}
	if want := []string{"local", "linux", "linux-unknown", "windows"}; !slices.Equal(order, want) {
		t.Fatalf("agent order %v, want %v", order, want)
	}
}

func TestConversationAgentPickerOpensRemoteChatThroughGateway(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatalf("read test hostname: %q, %v", host, err)
	}
	localOS := runtime.GOOS
	if localOS == "darwin" {
		localOS = "macos"
	}
	remoteHost := "remote-linux"
	remote := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	local := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: localOS}
	chat := storage.Conversation{ID: uuid.NewString(), ProjectIdent: "demo", Provider: remote.Provider, Owner: remote}
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		next = withoutComposerNavigationStream(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/projects/demo/worker-presence":
				_ = json.NewEncoder(w).Encode([]storage.AgentPresence{
					{State: "idle", PresenceAgent: storage.PresenceAgent{ProjectIdent: "demo", Hostname: &remoteHost, Origin: remote}},
					{State: "idle", PresenceAgent: storage.PresenceAgent{ProjectIdent: "demo", Hostname: &host, Origin: local}},
				})
			case "/v1/projects/demo/fleet-agents/" + remote.SessionID + "/conversation":
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				_ = json.NewEncoder(w).Encode(storage.FleetConversation{Conversation: chat, Hostname: remoteHost})
			default:
				next.ServeHTTP(w, r)
			}
		})
	})
	b := fixture.browser
	b.clientConfigPath = t.TempDir() // A managed desktop normally routes local chats through this config.
	b.setMainView("Conversations")
	waitBrowser(t, fixture.callbacks, func() bool { return len(b.conversationAgents.Objects) == 4 })
	rows := b.conversationAgents.Objects
	if rows[0].(*canvas.Text).Text != "This machine" || rows[2].(*canvas.Text).Text != "linux" {
		t.Fatal("expected the local heading above the Linux heading")
	}
	fynetest.Tap(rows[3].(*fyne.Container).Objects[0].(*widget.Button))
	waitBrowser(t, fixture.callbacks, func() bool { return b.selection.ProjectConversation == chat.ID })
	opened := b.chats[chatKey(chat.ProjectIdent, chat.ID)]
	if opened == nil || opened.client != b.client {
		t.Fatal("remote chat did not use the gateway transport")
	}
}
