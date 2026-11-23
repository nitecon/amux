package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type editorBrowserFixture struct {
	store         *storage.Store
	first, second *storage.Task
	browser       *Browser
	callbacks     *uiCallbacks
}

func startEditorBrowser(t *testing.T, wrap func(http.Handler) http.Handler) editorBrowserFixture {
	t.Helper()
	app := fynetest.NewApp()
	app.Settings().SetTheme(WorkspaceTheme())
	t.Cleanup(app.Quit)
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "editor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	initialSpec := "Initial specification"
	first, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "First", Reporter: "user", Details: &initialSpec}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Second", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "editor-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(wrap(handler))
	t.Cleanup(host.Close)
	t.Cleanup(func() { handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx) })
	client, err := gatewayclient.New(host.URL, "editor-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })
	// These fixtures exercise task details across workflow status changes.
	browser.statusSelect.SetSelected("All")
	browser.Start(ctx)
	waitBrowser(t, callbacks, func() bool { return len(browser.taskItems) == 2 })
	browser.selectTask(first.ID)
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.Task.ID == first.ID
	})
	return editorBrowserFixture{store: store, first: first, second: second, browser: browser, callbacks: callbacks}
}

// Rust desktop_tasks.rs::posting_a_comment_keeps_metadata_drafts_until_explicit_save_or_cancel,
// adapted to task-linked chat and server-driven detail refreshes.
func TestBrowserEditorPreservesDraftAndSavesOnlyChangedFields(t *testing.T) {
	patches := make(chan map[string]json.RawMessage, 4)
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read error", 400)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					http.Error(w, "invalid edit", 400)
					return
				}
				patches <- fields
			}
			handler.ServeHTTP(w, r)
		})
	})
	ctx := context.Background()
	store, first, second, browser, callbacks := fixture.store, fixture.first, fixture.second, fixture.browser, fixture.callbacks
	editor := browser.taskEditor
	editor.titleEntry.SetText("Unsaved title")
	remoteSpec := json.RawMessage(`"Specification from another user"`)
	if _, err := store.UpdateTaskWorkflow(ctx, "demo", first.ID, storage.TaskWorkflowUpdate{TaskMetadataUpdate: storage.TaskMetadataUpdate{Specification: remoteSpec}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTaskComment(ctx, "demo", first.ID, "user", "user", "New external comment", nil, nil); err != nil {
		t.Fatal(err)
	}
	browser.Refresh()
	waitBrowser(t, callbacks, func() bool { return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.CommentCount == 1 })
	if editor.titleEntry.Text != "Unsaved title" || editor.specificationEntry.Text != "Specification from another user" {
		t.Fatalf("refresh lost draft/fresh fields: %q %q", editor.titleEntry.Text, editor.specificationEntry.Text)
	}
	browser.selectTask(second.ID)
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.Task.ID == second.ID
	})
	if editor.titleEntry.Text != "Second" {
		t.Fatalf("wrong task editor: %q", editor.titleEntry.Text)
	}
	browser.selectTask(first.ID)
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.Task.ID == first.ID
	})
	if editor.titleEntry.Text != "Unsaved title" {
		t.Fatalf("navigation lost draft: %q", editor.titleEntry.Text)
	}
	fynetest.Tap(editor.save)
	waitBrowser(t, callbacks, func() bool {
		detail, err := store.Task(ctx, "demo", first.ID)
		return err == nil && detail.Task.Title == "Unsaved title"
	})
	select {
	case fields := <-patches:
		if len(fields) != 1 || string(fields["title"]) != `"Unsaved title"` {
			t.Fatalf("save sent untouched fields: %v", fields)
		}
	default:
		t.Fatal("no PATCH captured")
	}
	detail, err := store.Task(ctx, "demo", first.ID)
	if err != nil || detail.Task.Details == nil || *detail.Task.Details != "Specification from another user" {
		t.Fatalf("save overwrote unrelated edit: %+v %v", detail, err)
	}
	waitBrowser(t, callbacks, func() bool { return !editor.titleEntry.Disabled() })
	editor.titleEntry.SetText("Discard this draft")
	fynetest.Tap(editor.cancel)
	if editor.titleEntry.Text != "Unsaved title" {
		t.Fatalf("Cancel did not restore server value: %q", editor.titleEntry.Text)
	}
	captureTaskEditor(t, browser)
}

func captureTaskEditor(t *testing.T, browser *Browser) {
	t.Helper()
	window := fynetest.NewWindow(browser.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	accordion := findBrowserAccordion(browser.Content(), "Task settings")
	if accordion == nil {
		t.Fatal("missing task details accordion")
	}
	accordion.Open(0)
	window.Resize(fyne.NewSize(1280, 820))
	browser.Content().Refresh()
	file, err := os.Create(filepath.Join(t.TempDir(), "amux-task-editor-preview.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, window.Canvas().Capture()); err != nil {
		t.Fatal(err)
	}
}

func findBrowserAccordion(object fyne.CanvasObject, title string) *widget.Accordion {
	switch value := object.(type) {
	case *widget.Accordion:
		for _, item := range value.Items {
			if item.Title == title {
				return value
			}
		}
	case *container.Split:
		if found := findBrowserAccordion(value.Leading, title); found != nil {
			return found
		}
		return findBrowserAccordion(value.Trailing, title)
	case *container.Scroll:
		return findBrowserAccordion(value.Content, title)
	case *fyne.Container:
		for _, child := range value.Objects {
			if found := findBrowserAccordion(child, title); found != nil {
				return found
			}
		}
	}
	return nil
}

func TestBrowserEditorDelayedSaveKeepsNewerDetailAndOtherTask(t *testing.T) {
	committed, allowReply := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(allowReply) })
	defer release()
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				handler.ServeHTTP(w, r)
				return
			}
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			close(committed)
			select {
			case <-allowReply:
			case <-r.Context().Done():
				return
			}
			for key, values := range recorded.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
		})
	})
	browser, callbacks, editor := fixture.browser, fixture.callbacks, fixture.browser.taskEditor
	editor.titleEntry.SetText("Saved title")
	fynetest.Tap(editor.save)
	waitBrowser(t, callbacks, func() bool {
		select {
		case <-committed:
			return true
		default:
			return false
		}
	})
	if _, err := fixture.store.UpdateTaskWorkflow(context.Background(), "demo", fixture.first.ID, storage.TaskWorkflowUpdate{TaskMetadataUpdate: storage.TaskMetadataUpdate{Specification: json.RawMessage(`"Newer remote specification"`)}}, nil); err != nil {
		t.Fatal(err)
	}
	browser.Refresh()
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.Task.Details != nil && *browser.selectedTaskDetail.Task.Details == "Newer remote specification"
	})
	browser.selectTask(fixture.second.ID)
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.Task.ID == fixture.second.ID
	})
	release()
	sourceKey := taskEditorKey("demo", fixture.first.ID)
	waitBrowser(t, callbacks, func() bool { return !editor.drafts[sourceKey].inFlight })
	if editor.titleEntry.Text != "Second" {
		t.Fatalf("late save corrupted selected task: %q", editor.titleEntry.Text)
	}
	if editor.drafts[sourceKey].specification != "Newer remote specification" {
		t.Fatalf("late save regressed newer detail: %q", editor.drafts[sourceKey].specification)
	}
}
