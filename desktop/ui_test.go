package desktop

import (
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/chat"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

const (
	testProject = "demo"
	testTask    = "task-1"
	testConv    = "conversation-1"
	testSession = "native-session-1"
)

type receivedCommand struct {
	ID      string               `json:"id"`
	Command conversation.Command `json:"command"`
}

type fakeGateway struct {
	t             *testing.T
	server        *httptest.Server
	mu            sync.Mutex
	commands      []receivedCommand
	firstPromptID string
	firstPrompt   bool
}

type uiCallbacks struct{ pending chan func() }

func activeTaskListLoading(b *Browser) bool {
	state := b.taskLists[b.activeWorkspaceID]
	return state != nil && state.loading
}

func newUICallbacks() *uiCallbacks { return &uiCallbacks{pending: make(chan func(), 256)} }

func (q *uiCallbacks) dispatch(fn func()) { q.pending <- fn }

func (q *uiCallbacks) pump() {
	for {
		select {
		case fn := <-q.pending:
			fn()
		default:
			return
		}
	}
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{t: t}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v1/projects/"+testProject+"/conversations/"+testConv:
		_ = json.NewEncoder(w).Encode(storage.Conversation{ID: testConv, ProjectIdent: testProject, TaskID: testTask})
	case path == "/v1/projects/"+testProject+"/tasks/"+testTask:
		_ = json.NewEncoder(w).Encode(storage.Task{ID: testTask, ProjectIdent: testProject, Title: "Desktop chat task"})
	case path == "/v1/projects/"+testProject+"/conversations/"+testConv+"/stream":
		g.stream(w, r)
	case path == "/v1/projects/"+testProject+"/conversations/"+testConv+"/commands":
		var command receivedCommand
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			http.Error(w, "bad command", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.commands = append(g.commands, command)
		if command.Command.Kind == "prompt" && !g.firstPrompt {
			g.firstPrompt = true
			g.firstPromptID = command.ID
			g.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable) // Simulate a lost ACK after server acceptance.
			return
		}
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGateway) stream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	owner := storage.AgentOrigin{SessionID: "session-owner", InstanceID: "instance-owner", Provider: "codex", OS: "darwin"}
	if err = wsjson.Write(ctx, conn, gatewayclient.Frame{Type: "ready", Version: 1, ConversationID: testConv, Owner: &owner}); err != nil {
		return
	}
	for {
		if _, _, err = conn.Read(ctx); err != nil {
			return
		}
	}
}

func newFixtureModel(t *testing.T) *chat.Model {
	t.Helper()
	model := chat.New()
	events := []conversation.Event{
		{Kind: "user_message", SessionID: testSession, MessageID: "user-1", Text: "Find the config file"},
		{Kind: "tool_call", SessionID: testSession, ToolCallID: "tool-1", Title: "Bash", Data: json.RawMessage(`{"content":[{"type":"tool_use","id":"tool-1","input":{"command":"rg --files --glob '*.go' --glob '!**/vendor/**' --glob '!**/testdata/**' --hidden --no-ignore path-at-end"}}]}`)},
		{Kind: "permission", SessionID: testSession, RequestID: "request-1", ToolCallID: "tool-1", Title: "Run the search?", Data: json.RawMessage(`{"command":"rg --files","directory":"/repo"}`), Options: []conversation.PermissionOption{{ID: "allow", Name: "Allow once"}, {ID: "deny", Name: "Deny"}}},
		{Kind: "text_delta", SessionID: testSession, MessageID: "assistant-1", Text: "I will inspect the repository."},
		{Kind: "state", SessionID: testSession, Status: "message_stop"},
		{Kind: "tool_update", SessionID: testSession, ToolCallID: "tool-1", Status: "completed", Data: json.RawMessage(`"found-config.go\nfound-settings.go"`)},
	}
	for i, event := range events {
		if err := model.Apply(storage.ConversationEvent{ID: "event-" + string(rune('1'+i)), ConversationID: testConv, Sequence: int64(i + 1), Event: event}); err != nil {
			t.Fatalf("apply fixture event %d: %v", i+1, err)
		}
	}
	return model
}

func TestChatUIRendersAndSendsCommands(t *testing.T) {
	fynetest.NewApp().Settings().SetTheme(WorkspaceTheme())
	gateway := newFakeGateway(t)
	client, err := gatewayclient.New(gateway.server.URL, "test-key", gateway.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	ui, err := New(Config{Client: client, Project: testProject, ConversationID: testConv, Model: newFixtureModel(t), Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	window := fynetest.NewWindow(ui.Content())
	window.Resize(fyne.NewSize(1040, 760))
	defer func() {
		ui.Close()
		ui.Wait()
		callbacks.pump()
		window.Close()
	}()

	markup := fynetest.RenderToMarkup(window.Canvas())
	if !strings.Contains(markup, "Inspect details") || !strings.Contains(markup, "rg --files") ||
		strings.Contains(markup, "path-at-end") || strings.Contains(markup, "found-config.go") ||
		strings.Contains(markup, "message_stop") {
		t.Fatalf("tool details should start collapsed; markup: %s", markup)
	}
	if !strings.Contains(markup, "Run the search?") || !strings.Contains(markup, "Allow once") || !strings.Contains(markup, "Find the config file") || !strings.Contains(markup, "Inspect request details") || strings.Contains(markup, `"directory": "/repo"`) {
		t.Fatalf("transcript or permission controls missing: %s", markup)
	}
	if err := writeHUDPNG(window.Canvas(), filepath.Join(t.TempDir(), "amux-desktop-preview.png")); err != nil {
		t.Fatalf("write visual preview: %v", err)
	}
	captureHUDState(t, "tool-collapsed", window.Canvas())

	fynetest.Tap(ui.toolButtons["tool-1"])
	callbacks.pump()
	if expanded := fynetest.RenderToMarkup(window.Canvas()); !strings.Contains(expanded, "path-at-end") || !strings.Contains(expanded, "found-config.go") {
		t.Fatal("expanded tool call does not show its details")
	}
	captureHUDState(t, "tool-expanded", window.Canvas())
	fynetest.Tap(ui.permissionDetailButtons["event-3"])
	callbacks.pump()
	if expanded := fynetest.RenderToMarkup(window.Canvas()); !strings.Contains(expanded, `"directory": "/repo"`) {
		t.Fatal("permission request details were not inspectable")
	}

	ui.Start(context.Background())
	waitForUI(t, callbacks, 3*time.Second, func() bool { return ui.heading.Text == "Desktop chat task · conversa" })
	ui.composer.SetText(" ")
	ui.composer.TypedKey(&fyne.KeyEvent{Name: fyne.KeyReturn})
	ui.composer.typedKey(&fyne.KeyEvent{Name: fyne.KeyEnter}, fyne.KeyModifierShift)
	if ui.Snapshot().Draft != "\n " {
		t.Fatalf("Shift+Enter did not insert a newline: %q", ui.Snapshot().Draft)
	}
	ui.composer.SetText("  Please inspect the task  ")
	ui.composer.TypedKey(&fyne.KeyEvent{Name: fyne.KeyReturn})
	waitForUI(t, callbacks, 4*time.Second, func() bool {
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		ui.mu.Lock()
		pending := ui.pendingPrompt != ""
		ui.mu.Unlock()
		return len(gateway.commands) >= 2 && !pending && ui.Snapshot().Draft == ""
	})
	ui.mu.Lock()
	if ui.activePrompt != "" {
		t.Fatalf("enqueue acknowledgement must not mark a prompt active: %q", ui.activePrompt)
	}
	ui.mu.Unlock()
	gateway.mu.Lock()
	if gateway.commands[0].ID == "" || gateway.commands[0].ID != gateway.commands[1].ID {
		t.Fatalf("lost-ack retry changed command ID: %#v", gateway.commands[:2])
	}
	if gateway.commands[0].Command.Text != "Please inspect the task" || ui.Snapshot().Draft != "" {
		t.Fatalf("accepted prompt should clear only its unchanged draft")
	}
	gateway.mu.Unlock()

	fynetest.Tap(ui.permissionButtons["event-3/allow"])
	waitForUI(t, callbacks, 3*time.Second, func() bool {
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		return len(gateway.commands) >= 3
	})
	gateway.mu.Lock()
	permission := gateway.commands[2]
	gateway.mu.Unlock()
	if permission.Command.Kind != "permission" || permission.Command.PermissionEventID != "event-3" || permission.Command.OptionID != "allow" {
		t.Fatalf("permission callback sent wrong intent: %#v", permission)
	}
}

func TestOwnerAndCommandFramesKeepOnlyClaimedPromptActive(t *testing.T) {
	fynetest.NewApp()
	client, err := gatewayclient.New("http://localhost", "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	ui, err := New(Config{Client: client, Project: testProject, ConversationID: testConv, Model: newFixtureModel(t), Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ui.Close()
		ui.Wait()
		callbacks.pump()
	}()
	ui.mu.Lock()
	ui.cursor = gatewayclient.Cursor{Events: 4, Commands: 7}
	ui.activePrompt = "claimed-1"
	ui.mu.Unlock()
	if err := ui.consume(gatewayclient.Frame{Type: "command", Command: &storage.ConversationCommand{Revision: 8, ID: "queued-2", Command: conversation.Command{Kind: "prompt"}, State: "queued"}}); err != nil {
		t.Fatal(err)
	}
	callbacks.pump()
	ui.mu.Lock()
	active := ui.activePrompt
	ui.mu.Unlock()
	if active != "claimed-1" {
		t.Fatalf("queued prompt replaced active prompt: %q", active)
	}
	if err := ui.consume(gatewayclient.Frame{Type: "command", Command: &storage.ConversationCommand{Revision: 9, ID: "claimed-2", Command: conversation.Command{Kind: "prompt"}, State: "claimed"}}); err != nil {
		t.Fatal(err)
	}
	callbacks.pump()
	ui.mu.Lock()
	active = ui.activePrompt
	ui.mu.Unlock()
	if active != "claimed-1" {
		t.Fatalf("later claimed prompt replaced the current prompt: %q", active)
	}
	sequence := ui.Snapshot().Sequence
	if err := ui.consume(gatewayclient.Frame{Type: "owner"}); err != nil {
		t.Fatal(err)
	}
	callbacks.pump()
	if snapshot := ui.Snapshot(); len(snapshot.Permissions) != 0 || snapshot.Sequence != sequence {
		t.Fatalf("owner frame should clear ephemeral approvals without advancing history: %#v", snapshot)
	}
	ui.mu.Lock()
	active, cursor := ui.activePrompt, ui.cursor
	ui.mu.Unlock()
	if active != "" || cursor.Events != 4 || cursor.Commands != 9 {
		t.Fatalf("owner frame should clear cancel target without changing durable event cursor: active=%q cursor=%#v", active, cursor)
	}
}

func writeHUDPNG(canvas fyne.Canvas, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return png.Encode(file, canvas.Capture())
}

func captureHUDState(t *testing.T, name string, canvas fyne.Canvas) {
	t.Helper()
	directory := os.Getenv("AMUX_HUD_SNAPSHOTS")
	if directory == "" {
		return
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeHUDPNG(canvas, filepath.Join(directory, name+".png")); err != nil {
		t.Fatal(err)
	}
}

func waitForUI(t *testing.T, callbacks *uiCallbacks, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		callbacks.pump()
		if condition() {
			callbacks.pump()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	callbacks.pump()
	if !condition() {
		t.Fatal("timed out waiting for asynchronous UI action")
	}
}

func TestTranscriptReplayCoalescesUpdatesAndKeepsHistoryPageable(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	gateway := newFakeGateway(t)
	client, err := gatewayclient.New(gateway.server.URL, "fixture-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	queue := newUICallbacks()
	view, err := New(Config{Client: client, Project: testProject, ConversationID: testConv, Dispatch: queue.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= conversationPageSize*3; i++ {
		event := storage.ConversationEvent{
			ID: strconv.Itoa(i), ConversationID: testConv, Sequence: int64(i),
			Event: conversation.Event{Kind: "user_message", SessionID: testSession, Text: strconv.Itoa(i)},
		}
		if err := view.consume(gatewayclient.Frame{Type: "event", Event: &event}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(queue.pending); got != 1 {
		t.Fatalf("replay scheduled %d UI refreshes, want one", got)
	}
	queue.pump()
	if got := len(view.transcript.Objects); got != conversationPageSize+1 {
		t.Fatalf("rendered %d transcript objects, want one page and navigation", got)
	}
	view.transcript.Objects[0].(*widget.Button).OnTapped()
	if view.transcriptEnd != 2*conversationPageSize {
		t.Fatal("older message page was not selected")
	}
	view.transcript.Objects[len(view.transcript.Objects)-1].(*widget.Button).OnTapped()
	if view.transcriptEnd != -1 || len(view.Snapshot().Items) != 3*conversationPageSize {
		t.Fatal("returning to the latest page lost history")
	}
}

func TestTranscriptTextWrapsInsteadOfWideningThread(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	client, err := gatewayclient.New("http://127.0.0.1:1", "fixture-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := New(Config{Client: client, Project: testProject, ConversationID: testConv, TaskTitle: strings.Repeat("title ", 80), Dispatch: func(f func()) { f() }})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("unwrapped conversation text ", 40)
	objects := []fyne.CanvasObject{
		view.renderItem(chat.Item{Kind: "state", Text: long}),
		view.renderItem(chat.Item{Kind: "message", Role: "assistant", Text: long, Status: long}),
		view.renderPermission(chat.Permission{EventID: "p1", Title: long}),
		view.Content(),
	}
	for i, object := range objects {
		if width := object.MinSize().Width; width > 400 {
			t.Fatalf("object %d needs width %.0f; long text must wrap", i, width)
		}
	}
}
