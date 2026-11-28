package desktop

import (
	"context"
	"net/http"
	"testing"

	"fyne.io/fyne/v2"
	"github.com/google/uuid"
	"github.com/nitecon/amux/identity/agent"
)

func TestBrowserTaskWindowDetachRedockPreservesState(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	b, callbacks := fixture.browser, fixture.callbacks
	b.taskWindowFactory = fyne.CurrentApp().NewWindow
	owner := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	conversation, err := fixture.store.CreateConversation(context.Background(), "demo", fixture.first.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	b.selectTask(fixture.second.ID)
	waitBrowser(t, callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID })
	b.selectTask(fixture.first.ID)
	waitBrowser(t, callbacks, func() bool {
		return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID && len(b.conversationItems) == 1 && b.conversationItems[0].ID == conversation.ID
	})
	b.openConversation(*conversation)
	chat := b.chats[chatKey("demo", conversation.ID)]
	if chat == nil {
		t.Fatal("synthetic task conversation did not open")
	}
	waitBrowser(t, callbacks, func() bool {
		chat.mu.Lock()
		defer chat.mu.Unlock()
		return chat.started && chat.ctx != nil
	})
	chat.mu.Lock()
	chatContext := chat.ctx
	chat.mu.Unlock()
	b.taskEditor.titleEntry.SetText("Unsaved task title")
	draft := b.taskWorkflow.currentDraft()
	draft.reviewAction = "Keep this workflow draft"
	state := b.taskLists[b.activeWorkspaceID]
	editor, workflow, chatHost := b.taskEditor, b.taskWorkflow, b.chatHost
	chatCount := len(b.chats)

	b.toggleTaskWindow()
	window := b.taskWindow
	if window == nil || b.taskPaneHost.Trailing == b.taskPane {
		t.Fatal("task content did not move into a child window")
	}
	if window.Content() != b.taskPane {
		t.Fatal("child window does not own the existing task pane")
	}
	if b.selectedTask != fixture.first.ID || b.taskEditor.titleEntry.Text != "Unsaved task title" || b.taskWorkflow.currentDraft().reviewAction != "Keep this workflow draft" {
		t.Fatal("detaching changed selected task or its drafts")
	}
	if b.taskLists[b.activeWorkspaceID] != state || b.taskEditor != editor || b.taskWorkflow != workflow || b.chatHost != chatHost || len(b.chats) != chatCount || b.chats[chatKey("demo", conversation.ID)] != chat {
		t.Fatal("detaching replaced Browser task or conversation runtime state")
	}
	chat.mu.Lock()
	chatContextPreserved := chat.ctx == chatContext && chat.started && !chat.closing
	chat.mu.Unlock()
	if !chatContextPreserved {
		t.Fatal("detaching interrupted the live task conversation")
	}

	window.Close()
	callbacks.pump()
	if b.taskWindow != nil || b.taskPaneHost.Trailing != b.taskPane {
		t.Fatal("native child close did not redock tasks into the parent")
	}
	if b.selectedTask != fixture.first.ID || b.taskEditor.titleEntry.Text != "Unsaved task title" || b.taskWorkflow.currentDraft().reviewAction != "Keep this workflow draft" {
		t.Fatal("redocking changed selected task or its drafts")
	}
	chat.mu.Lock()
	chatContextPreserved = chat.ctx == chatContext && chat.started && !chat.closing
	chat.mu.Unlock()
	if !chatContextPreserved {
		t.Fatal("redocking interrupted the live task conversation")
	}

	b.toggleTaskWindow()
	if b.taskWindow == nil {
		t.Fatal("tasks could not be detached again")
	}
	child := b.taskWindow
	b.Close()
	callbacks.pump()
	b.toggleTaskWindow()
	if b.taskWindow != nil {
		t.Fatal("Browser shutdown left or reopened a child task window")
	}
	if child.Content() == b.taskPane {
		t.Fatal("Browser shutdown left the task pane attached to the child window")
	}
}
