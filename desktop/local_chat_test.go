package desktop

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/chat"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type markdownFixtureSession struct {
	sink conversation.Sink
	done chan struct{}
}

func (s *markdownFixtureSession) Done() <-chan struct{}                                 { return s.done }
func (*markdownFixtureSession) Err() error                                              { return nil }
func (*markdownFixtureSession) ResolvePermission(context.Context, string, string) error { return nil }
func (s *markdownFixtureSession) Close() error                                          { close(s.done); return nil }
func (s *markdownFixtureSession) Prompt(ctx context.Context, _ string) (string, error) {
	for _, text := range []string{"Local ", "reply"} {
		if err := s.sink(ctx, conversation.Event{Kind: "text_delta", SessionID: "fixture-native", MessageID: "reply", Text: text}); err != nil {
			return "", err
		}
	}
	return "", nil
}

func TestMarkdownConversationRunsAndRestoresWithoutGateway(t *testing.T) {
	root := t.TempDir()
	info, err := createMarkdownChat(root, "demo", root, "codex", ConversationLaunchPreferences{Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := listMarkdownChats(root, "demo")
	if err != nil || len(saved) != 1 || saved[0].ID != info.ID {
		t.Fatalf("list chats: %v, %v", saved, err)
	}
	local, err := openMarkdownChat(root, info, func(ctx context.Context, run storage.WorkerRun, sink conversation.Sink) (worker.Session, error) {
		if err := sink(ctx, conversation.Event{Kind: "session_ready", SessionID: "fixture-native"}); err != nil {
			return nil, err
		}
		return &markdownFixtureSession{sink: sink, done: make(chan struct{})}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	ready, finished := make(chan struct{}), make(chan struct{}, 1)
	model := chat.New()
	go func() {
		done <- local.Stream(ctx, "demo", info.ID, gatewayclient.Cursor{}, func(frame gatewayclient.Frame) error {
			switch frame.Type {
			case "local_connecting":
				return nil
			case "local_ready":
				close(ready)
				return nil
			case "local_failed":
				return errors.New(frame.Event.Event.Text)
			}
			if err := model.Apply(*frame.Event); err != nil {
				return err
			}
			if frame.Event.Event.Kind == "turn_completed" {
				finished <- struct{}{}
			}
			return nil
		})
	}()
	select {
	case <-ready:
		metadata, err := local.Conversation(ctx, "demo", info.ID)
		if err != nil || metadata.NativeSessionID != "fixture-native" {
			t.Fatal("provider must connect before the first prompt")
		}
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("stream did not start")
	}
	if err := local.Enqueue(ctx, "demo", info.ID, "prompt-id", conversation.Command{Kind: "prompt", Text: "Local question"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("prompt did not finish")
	}
	cancel()
	<-done
	path := filepath.Join(root, "demo", info.ID+".md")
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "> Local question") || !strings.Contains(string(raw), "> Local reply") {
		t.Fatalf("readable Markdown missing: %v\n%s", err, raw)
	}
	reopened, err := openMarkdownChat(root, info, nil)
	if err != nil {
		t.Fatal(err)
	}
	header, events := reopened.Snapshot()
	if header.Info.NativeSessionID != "fixture-native" || header.Preferences.Model != "fixture" || len(events) == 0 {
		t.Fatalf("resume metadata not restored: %+v", header)
	}
}

func TestBrowserCreatesLocalChatWithoutGatewayConversationRoutes(t *testing.T) {
	var calls atomic.Int32
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/projects/demo/conversations" {
				calls.Add(1)
				http.NotFound(w, r)
				return
			}
			handler.ServeHTTP(w, r)
		})
	})
	b := fixture.browser
	b.localConversationsDirectory = t.TempDir()
	b.setMainView("Conversations")
	fynetest.Tap(b.providerTabs["codex"])
	waitBrowser(t, fixture.callbacks, func() bool {
		label, ok := b.standaloneChatHost.Objects[0].(*widget.Label)
		return ok && label.Text == "Check out this project to start a local chat."
	})
	b.selection.CheckoutPath = t.TempDir()
	fynetest.Tap(b.providerTabs["codex"])
	waitBrowser(t, fixture.callbacks, func() bool {
		return len(b.conversationHistory) == 1 && b.selection.ProjectConversation != ""
	})
	old := b.selection.ProjectConversation
	fynetest.Tap(b.newChatButton)
	waitBrowser(t, fixture.callbacks, func() bool {
		return len(b.conversationHistory) == 2 && b.selection.ProjectConversation != old
	})
	for _, id := range []string{old, b.selection.ProjectConversation} {
		if _, err := os.Stat(filepath.Join(b.localConversationsDirectory, "demo", id+".md")); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("standalone chat accessed the gateway conversations endpoint")
	}
}
