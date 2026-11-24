package desktop

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestBrowserKeepsChatDraftAcrossTasksAndDiscoversNewConversations(t *testing.T) {
	a := fynetest.NewApp()
	a.Settings().SetTheme(WorkspaceTheme())
	t.Cleanup(a.Quit)
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "browser.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "First task", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Second task", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	c1, err := s.CreateConversation(ctx, "demo", first.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.CreateConversation(ctx, "demo", second.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSession(ctx, "demo", c1.ID, "browser-preview-native", &owner); err != nil {
		t.Fatal(err)
	}
	for _, event := range []conversation.Event{
		{Kind: "user_message", SessionID: "browser-preview-native", Text: "Review the Go migration"},
		{Kind: "text_delta", SessionID: "browser-preview-native", MessageID: "answer", Text: "Checking task state and native provider integration."},
		{Kind: "tool_call", SessionID: "browser-preview-native", ToolCallID: "inspect", Title: "Inspect changed files", Status: "completed", Data: json.RawMessage(`{"files":["storage/assignment.go","desktop/browser.go"]}`)},
		{Kind: "turn_completed", SessionID: "browser-preview-native", Status: "end_turn"},
	} {
		if _, err := s.AppendConversationEvent(ctx, "demo", c1.ID, uuid.NewString(), event, &owner); err != nil {
			t.Fatal(err)
		}
	}
	h, err := server.NewHandler(s, "browser-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	t.Cleanup(host.Close)
	t.Cleanup(func() { h.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx) })
	client, err := gatewayclient.New(host.URL, "browser-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	queue := newUICallbacks()
	b, err := NewBrowser(BrowserConfig{Client: client, Dispatch: queue.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	window := fynetest.NewWindow(b.Content())
	window.Resize(fyne.NewSize(1280, 820))
	t.Cleanup(window.Close)
	t.Cleanup(func() { b.Close(); b.Wait(); queue.pump() })
	b.Start(ctx)
	wait := func(label string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			queue.pump()
			if condition() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("browser did not %s", label)
	}
	wait("load queued tasks in the initial status", func() bool { return b.selectedProject == "demo" && len(b.visibleTasks) == 2 })
	captureHUDState(t, "queue", window.Canvas())
	selectTask := func(id string) {
		t.Helper()
		for i, task := range b.visibleTasks {
			if task.ID == id {
				b.tasks.Select(i)
				return
			}
		}
		t.Fatalf("task %s missing", id)
	}
	selectConversation := func(id string) {
		t.Helper()
		for _, c := range b.conversationItems {
			if c.ID == id {
				b.openConversation(c)
				return
			}
		}
		t.Fatalf("conversation %s missing", id)
	}
	selectTask(first.ID)
	wait("load first conversation", func() bool { return len(b.conversationItems) == 1 && b.conversationItems[0].ID == c1.ID })
	captureHUDState(t, "selected-task", window.Canvas())
	selectConversation(c1.ID)
	firstChat := b.chats[chatKey("demo", c1.ID)]
	if firstChat == nil {
		t.Fatal("first chat did not open")
	}
	firstChat.composer.SetText("Draft survives navigation")
	if b.taskDetailPage.Visible() || !b.taskChatPage.Visible() {
		t.Fatal("chat must replace the entire task detail pane")
	}
	b.backToTaskDetail()
	if !b.taskDetailPage.Visible() || b.taskChatPage.Visible() {
		t.Fatal("Back did not restore task details")
	}
	selectConversation(c1.ID)
	selectTask(first.ID)
	if !b.taskDetailPage.Visible() || b.taskChatPage.Visible() {
		t.Fatal("selecting the same task did not restore task details")
	}
	selectConversation(c1.ID)
	selectTask(second.ID)
	wait("load second conversation", func() bool { return len(b.conversationItems) == 1 && b.conversationItems[0].ID == c2.ID })
	selectConversation(c2.ID)
	if b.selectedConversation != c2.ID {
		t.Fatal("same-index conversation click did not open the new task chat")
	}
	selectTask(first.ID)
	wait("return to first task", func() bool { return len(b.conversationItems) == 1 && b.conversationItems[0].ID == c1.ID })
	selectConversation(c1.ID)
	if b.chats[chatKey("demo", c1.ID)] != firstChat || firstChat.composer.Text != "Draft survives navigation" {
		t.Fatal("navigation replaced the cached chat or its draft")
	}
	if _, err := s.CreateConversation(ctx, "demo", first.ID, &owner); err != nil {
		t.Fatal(err)
	}
	b.Refresh()
	wait("discover a new conversation on refresh", func() bool { return len(b.conversationItems) == 2 })
	if b.selectedConversation != c1.ID || firstChat.composer.Text != "Draft survives navigation" {
		t.Fatal("refresh discarded current chat selection or draft")
	}
	queue.pump()
	if err := writeHUDPNG(window.Canvas(), filepath.Join(t.TempDir(), "amux-browser-integration-preview.png")); err != nil {
		t.Fatal(err)
	}
	standalone, err := s.CreateConversation(ctx, "demo", "", &owner)
	if err != nil {
		t.Fatal(err)
	}
	rail, offset := b.root.Leading, b.root.Offset
	for i, conversation := range b.conversationItems {
		if conversation.ID == c1.ID {
			b.conversations.Select(i)
			break
		}
	}
	if b.mainView != "Conversations" || b.selection.ProjectConversation != c1.ID {
		t.Fatal("task conversation link did not open Task Thread")
	}
	wait("list task conversations and discover the default chat", func() bool {
		return len(b.projectConversationItems) == 3 && b.defaultChats["codex"].ID == standalone.ID
	})
	if b.root.Leading != rail || b.root.Offset != offset {
		t.Fatal("Conversations changed the Projects rail or its width")
	}
	for index, chat := range b.conversationHistory {
		if chat.ID == c1.ID {
			b.projectConversationList.Select(index)
			break
		}
	}
	if b.mainView != "Conversations" || b.standaloneChatHost.Objects[0] == firstChat.Content() {
		t.Fatal("linked chat must stay in Conversations with a task link")
	}
	fynetest.Tap(b.providerTabs["codex"])
	if b.mainView != "Conversations" || b.chats[chatKey("demo", standalone.ID)] == nil {
		t.Fatal("standalone chat did not open in the Conversations view")
	}
	if err := s.InsertProject(ctx, storage.Project{Ident: "other", ChannelName: "internal", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	b.setMainView("Tasks")
	b.selectProject("other")
	b.setMainView("Conversations")
	wait("load conversation list for the new project", func() bool { return !b.projectConversationsLoading })
	if len(b.projectConversationItems) != 0 {
		t.Fatalf("conversation list leaked across projects: %+v", b.projectConversationItems)
	}
	if _, empty := b.standaloneChatHost.Objects[0].(*widget.Label); !empty {
		t.Fatal("the previous project's transcript stayed open")
	}
}
