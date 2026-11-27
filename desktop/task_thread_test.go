package desktop

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

func taskThreadButtons(root fyne.CanvasObject) []*widget.Button {
	var findScroll func(fyne.CanvasObject) *container.Scroll
	findScroll = func(obj fyne.CanvasObject) *container.Scroll {
		switch node := obj.(type) {
		case *container.Scroll:
			if node.Direction == container.ScrollHorizontalOnly {
				return node
			}
			return findScroll(node.Content)
		case *fyne.Container:
			for _, child := range node.Objects {
				if scroll := findScroll(child); scroll != nil {
					return scroll
				}
			}
		case *container.ThemeOverride:
			return findScroll(node.Content)
		}
		return nil
	}
	scroll := findScroll(root)
	if scroll == nil {
		return nil
	}
	tabs, ok := scroll.Content.(*fyne.Container)
	if !ok {
		return nil
	}
	buttons := make([]*widget.Button, 0, len(tabs.Objects))
	for _, child := range tabs.Objects {
		if button, ok := child.(*widget.Button); ok {
			buttons = append(buttons, button)
		}
	}
	return buttons
}

func buttonLabels(buttons []*widget.Button) []string {
	labels := make([]string, len(buttons))
	for i, button := range buttons {
		labels[i] = button.Text
	}
	return labels
}

func TestTaskThreadHeaderLabelsAndOrderStayPinnedOnClick(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b := fixture.browser

	created := time.Date(2026, 11, 25, 21, 4, 0, 0, time.Local).UnixMilli()
	hostAlpha := "AlphaHost"
	hostBeta := "BetaHost"
	hostNewest := "ZuluHost"

	newest := storage.Conversation{
		ID:            uuid.NewString(),
		ProjectIdent:  "demo",
		TaskID:        fixture.first.ID,
		Provider:      "claude",
		Owner:         storage.AgentOrigin{SessionID: "session-newest", Provider: "claude"},
		OwnerHostname: &hostNewest,
		CreatedAt:     created,
		UpdatedAt:     created + 30_000,
	}
	tieBeta := storage.Conversation{
		ID:            uuid.NewString(),
		ProjectIdent:  "demo",
		TaskID:        fixture.first.ID,
		Provider:      "codex",
		Owner:         storage.AgentOrigin{SessionID: "session-beta", Provider: "codex"},
		OwnerHostname: &hostBeta,
		CreatedAt:     created,
		UpdatedAt:     created + 10_000,
	}
	tieAlpha := storage.Conversation{
		ID:            uuid.NewString(),
		ProjectIdent:  "demo",
		TaskID:        fixture.first.ID,
		Provider:      "antigravity",
		Owner:         storage.AgentOrigin{SessionID: "session-alpha", Provider: "antigravity"},
		OwnerHostname: &hostAlpha,
		CreatedAt:     created,
		UpdatedAt:     created + 10_000,
	}
	oldestNoHost := storage.Conversation{
		ID:           uuid.NewString(),
		ProjectIdent: "demo",
		TaskID:       fixture.first.ID,
		Provider:     "codex",
		Owner:        storage.AgentOrigin{SessionID: "session-oldest", Provider: "codex"},
		CreatedAt:    created - 60_000,
	}

	// Put them in an unsorted order and omit them from b.conversationItems to
	// simulate going directly to the Conversations tab.
	b.conversationItems = nil
	b.conversationHistory = []storage.Conversation{oldestNoHost, tieBeta, newest, tieAlpha}

	wantLabels := []string{
		conversationLabel(newest),
		conversationLabel(tieAlpha),
		conversationLabel(tieBeta),
		conversationLabel(oldestNoHost),
	}

	// Opening a non-leftmost thread must NOT snap it to the left.
	b.openStandaloneConversation(tieBeta)
	buttons := taskThreadButtons(b.standaloneChatHost.Objects[0])
	if got := buttonLabels(buttons); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("initial task thread labels = %#v, want %#v", got, wantLabels)
	}
	if buttons[0].Importance == widget.HighImportance || buttons[2].Importance != widget.HighImportance {
		t.Fatalf("expected index 2 (%q) to be active without snapping left", buttons[2].Text)
	}

	// Clicking the rightmost thread must highlight it in place while keeping the
	// most recently updated agent pinned to the left.
	fynetest.Tap(buttons[3])
	if b.selection.ProjectConversation != oldestNoHost.ID {
		t.Fatalf("selected conversation = %q, want %q", b.selection.ProjectConversation, oldestNoHost.ID)
	}
	buttons = taskThreadButtons(b.standaloneChatHost.Objects[0])
	if got := buttonLabels(buttons); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("after click task thread labels = %#v, want %#v", got, wantLabels)
	}
	if buttons[0].Importance == widget.HighImportance || buttons[3].Importance != widget.HighImportance {
		t.Fatalf("expected index 3 (%q) to be active without snapping left", buttons[3].Text)
	}

	// Switching tasks in Tasks view must not lose sibling threads on the active
	// task thread in Conversations view.
	b.conversationItems = []storage.Conversation{{
		ID:           uuid.NewString(),
		ProjectIdent: "demo",
		TaskID:       fixture.second.ID,
		Provider:     "claude",
		Owner:        storage.AgentOrigin{SessionID: "other-task-agent", Provider: "claude"},
		CreatedAt:    created,
	}}
	b.conversationHistory = nil
	fynetest.Tap(buttons[1])
	buttons = taskThreadButtons(b.standaloneChatHost.Objects[0])
	if got := buttonLabels(buttons); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("after selecting another task, labels = %#v, want %#v", got, wantLabels)
	}
	if buttons[1].Importance != widget.HighImportance {
		t.Fatalf("expected index 1 (%q) to be active", buttons[1].Text)
	}
}

func TestDirectConversationsTabShowsTaskThreadNamesAndOrder(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return withoutComposerNavigationStream(next)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hostOne := "Defcon2"
	hostTwo := "WillsM4Max.local"
	agentOne := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	agentTwo := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "macos"}
	if accepted, err := fixture.store.ReportPresence(ctx, storage.PresenceReport{
		Source: "pool-1", Sequence: 1, Hostname: &hostOne,
		Agents: []storage.PresenceAgent{{ProjectIdent: "demo", Origin: agentOne}},
	}, time.Now().UTC()); err != nil || !accepted {
		t.Fatalf("presence one: accepted=%v err=%v", accepted, err)
	}
	if accepted, err := fixture.store.ReportPresence(ctx, storage.PresenceReport{
		Source: "pool-2", Sequence: 1, Hostname: &hostTwo,
		Agents: []storage.PresenceAgent{{ProjectIdent: "demo", Origin: agentTwo}},
	}, time.Now().UTC()); err != nil || !accepted {
		t.Fatalf("presence two: accepted=%v err=%v", accepted, err)
	}

	firstCreated, err := fixture.store.CreateConversation(ctx, "demo", fixture.first.ID, &agentOne)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.BindConversationSession(ctx, "demo", firstCreated.ID, "native-one", &agentOne); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	secondCreated, err := fixture.store.CreateConversation(ctx, "demo", fixture.first.ID, &agentTwo)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := fixture.store.AppendConversationEvent(ctx, "demo", firstCreated.ID, uuid.NewString(),
		conversation.Event{Kind: "text_delta", SessionID: "native-one", Text: "latest update"}, &agentOne); err != nil {
		t.Fatal(err)
	}

	for _, localDir := range []string{"", t.TempDir()} {
		name := "gateway"
		if localDir != "" {
			name = "local_chats_dir"
		}
		t.Run(name, func(t *testing.T) {
			b := fixture.browser
			b.localConversationsDirectory = localDir
			b.conversationItems = nil
			b.taskThreadItems = nil
			b.selection.ProjectConversation = secondCreated.ID
			b.setMainView("Conversations")
			waitBrowser(t, fixture.callbacks, func() bool {
				return !b.projectConversationsLoading && len(b.standaloneChatHost.Objects) == 1 &&
					len(taskThreadButtons(b.standaloneChatHost.Objects[0])) == 2
			})

			buttons := taskThreadButtons(b.standaloneChatHost.Objects[0])
			listed, err := fixture.store.TaskConversations(ctx, "demo", fixture.first.ID, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			sortTaskThreads(listed)
			want := []string{conversationLabel(listed[0]), conversationLabel(listed[1])}
			if listed[0].ID != firstCreated.ID || listed[1].ID != secondCreated.ID {
				t.Fatalf("expected most recently updated (%s) first, got %+v", firstCreated.ID, listed)
			}
			if got := buttonLabels(buttons); !reflect.DeepEqual(got, want) {
				t.Fatalf("direct Conversations tab thread labels = %#v, want %#v", got, want)
			}
			if buttons[0].Importance == widget.HighImportance || buttons[1].Importance != widget.HighImportance {
				t.Fatalf("expected older-updated thread at index 1 to stay active without snapping left")
			}
			fynetest.Tap(buttons[0])
			buttons = taskThreadButtons(b.standaloneChatHost.Objects[0])
			if got := buttonLabels(buttons); !reflect.DeepEqual(got, want) {
				t.Fatalf("after clicking leftmost thread, labels = %#v, want %#v", got, want)
			}
			if buttons[0].Importance != widget.HighImportance {
				t.Fatalf("expected leftmost thread to be active after tap")
			}
		})
	}
}
