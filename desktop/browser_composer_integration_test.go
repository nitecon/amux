package desktop

import (
	"context"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

// Hold the acknowledgement after the real API commits, rather than delaying
// before the operation. Detail refreshes may observe the new row in this gap.
type committedTaskResponse struct {
	method, path string
	entered      chan struct{}
	release      chan struct{}
	once         sync.Once
	calls        atomic.Int32
}

func newCommittedTaskResponse(t *testing.T, method, path string) *committedTaskResponse {
	t.Helper()
	g := &committedTaskResponse{method: method, path: path, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.unblock)
	return g
}

func (g *committedTaskResponse) unblock() { g.once.Do(func() { close(g.release) }) }

func (g *committedTaskResponse) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != g.method || r.URL.Path != g.path {
			next.ServeHTTP(w, r)
			return
		}
		result := httptest.NewRecorder()
		next.ServeHTTP(result, r)
		if g.calls.Add(1) == 1 {
			close(g.entered)
			select {
			case <-g.release:
			case <-r.Context().Done():
				return
			}
		}
		for key, values := range result.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(result.Code)
		_, _ = w.Write(result.Body.Bytes())
	})
}

func (g *committedTaskResponse) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("task request did not commit")
	}
}

func TestBrowserCommentComposerPreservesMetadataUntilSaveOrCancel(t *testing.T) {
	for _, save := range []bool{false, true} {
		name := "cancel"
		if save {
			name = "save"
		}
		t.Run(name, func(t *testing.T) {
			fixture := startEditorBrowser(t, func(h http.Handler) http.Handler { return h })
			c, e := fixture.browser.taskComposer, fixture.browser.taskEditor
			e.titleEntry.SetText("Unsaved title")
			e.descriptionEntry.SetText("Unsaved description")
			e.specificationEntry.SetText("Unsaved specification")
			c.commentEntry.SetText("A human comment\nwith preserved lines")
			fynetest.Tap(c.commentSubmit)
			waitBrowser(t, fixture.callbacks, func() bool {
				return !c.commentDraft("demo", fixture.first.ID).inFlight && c.commentStatus.Text == "Comment posted."
			})
			if c.commentEntry.Text != "" {
				t.Fatalf("acknowledged comment retained submitted text: %q", c.commentEntry.Text)
			}
			if e.titleEntry.Text != "Unsaved title" || e.descriptionEntry.Text != "Unsaved description" || e.specificationEntry.Text != "Unsaved specification" {
				t.Fatal("posting changed unsaved metadata")
			}
			stored, err := fixture.store.Task(context.Background(), "demo", fixture.first.ID)
			if err != nil || stored.Task.Title != "First" || len(stored.Comments) != 1 {
				t.Fatalf("comment also saved metadata or duplicated: %+v %v", stored, err)
			}
			if stored.Comments[0].AuthorType != "user" || stored.Comments[0].Origin != nil {
				t.Fatalf("wrong comment provenance: %+v", stored.Comments[0])
			}
			if save {
				fynetest.Tap(e.save)
				waitBrowser(t, fixture.callbacks, func() bool {
					return !e.drafts[taskEditorKey("demo", fixture.first.ID)].inFlight && e.status.Text == "Task saved"
				})
				stored, err = fixture.store.Task(context.Background(), "demo", fixture.first.ID)
				if err != nil || stored.Task.Title != "Unsaved title" || stored.Task.Description == nil || *stored.Task.Description != "Unsaved description" || stored.Task.Details == nil || *stored.Task.Details != "Unsaved specification" {
					t.Fatalf("explicit save failed: %+v %v", stored, err)
				}
			} else {
				fynetest.Tap(e.cancel)
				if e.titleEntry.Text != "First" || e.descriptionEntry.Text != "" || e.specificationEntry.Text != "Initial specification" {
					t.Fatal("explicit cancel failed to restore metadata")
				}
			}
		})
	}
}

func TestBrowserCommentReceiptCannotClearNewerDraftOrAnotherTask(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodPost, "")
	fixture := startEditorBrowser(t, gate.wrap)
	gate.path = "/v1/projects/demo/tasks/" + fixture.first.ID + "/comments"
	b, c := fixture.browser, fixture.browser.taskComposer
	c.commentEntry.SetText("First submitted comment")
	fynetest.Tap(c.commentSubmit)
	gate.await(t)
	if !c.commentSubmit.Disabled() || c.commentEntry.Disabled() {
		t.Fatal("pending comment must block duplicate submit while allowing typing")
	}
	c.commentEntry.SetText("Newer unsent comment")
	b.selectTask(fixture.second.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID })
	c.commentEntry.SetText("Second task draft")
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return !c.commentDraft("demo", fixture.first.ID).inFlight })
	if b.selectedTask != fixture.second.ID || c.commentEntry.Text != "Second task draft" {
		t.Fatal("late receipt changed the current task or draft")
	}
	b.selectTask(fixture.first.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID })
	if c.commentEntry.Text != "Newer unsent comment" || gate.calls.Load() != 1 {
		t.Fatalf("newer draft lost or POST replayed: %q calls=%d", c.commentEntry.Text, gate.calls.Load())
	}
	stored, err := fixture.store.Task(context.Background(), "demo", fixture.first.ID)
	if err != nil || len(stored.Comments) != 1 || stored.Comments[0].Content != "First submitted comment" {
		t.Fatalf("unexpected committed comment: %+v %v", stored, err)
	}
}

func TestBrowserUncertainCommentRequiresExplicitSuccessfulRefresh(t *testing.T) {
	var posts atomic.Int32
	var failDetail atomic.Bool
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/navigation/stream" {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodGet && failDetail.Load() && strings.HasPrefix(r.URL.Path, "/v1/projects/demo/tasks/") && !strings.HasSuffix(r.URL.Path, "/conversations") {
				http.Error(w, "fixture refresh failure", 503)
				return
			}
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
				posts.Add(1)
				result := httptest.NewRecorder()
				next.ServeHTTP(result, r) // Commit before hiding the acknowledgement.
				http.Error(w, "fixture acknowledgement lost", 503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	b, c := fixture.browser, fixture.browser.taskComposer
	b.taskEditor.titleEntry.SetText("Unsaved local title")
	c.commentEntry.SetText("Keep this uncertain comment draft")
	fynetest.Tap(c.commentSubmit)
	draft := c.commentDraft("demo", fixture.first.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.inFlight && draft.unknown })
	c.submitComment()
	b.refreshTaskDetail() // Background and ordinary detail refreshes do not resolve a POST.
	waitBrowser(t, fixture.callbacks, func() bool { return !b.detailLoading && b.selectedTaskDetail.CommentCount == 1 })
	if !draft.unknown || c.commentEntry.Text != "Keep this uncertain comment draft" || !c.commentSubmit.Disabled() || posts.Load() != 1 {
		t.Fatal("ordinary refresh hid or repeated the uncertain comment")
	}
	failDetail.Store(true)
	fynetest.Tap(c.commentRefresh)
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.refreshing && strings.Contains(draft.status, "Refresh failed") })
	if !draft.unknown || !c.commentSubmit.Disabled() {
		t.Fatal("failed reconciliation unblocked the comment")
	}
	if _, err := fixture.store.AddTaskComment(context.Background(), "demo", fixture.first.ID, "user", "user", "An external comment after disconnect", nil, nil); err != nil {
		t.Fatal(err)
	}
	serverTitle := "Server title after disconnect"
	if _, err := fixture.store.UpdateTaskWorkflow(context.Background(), "demo", fixture.first.ID, storage.TaskWorkflowUpdate{TaskMetadataUpdate: storage.TaskMetadataUpdate{Title: &serverTitle}}, nil); err != nil {
		t.Fatal(err)
	}
	failDetail.Store(false)
	fynetest.Tap(c.commentRefresh)
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.refreshing && !draft.unknown })
	if c.commentEntry.Text != "Keep this uncertain comment draft" || c.commentSubmit.Disabled() || posts.Load() != 1 {
		t.Fatal("explicit refresh lost the draft or automatically posted it")
	}
	if b.selectedTaskDetail.CommentCount != 2 || b.selectedTaskDetail.Task.Title != serverTitle || b.taskEditor.titleEntry.Text != "Unsaved local title" {
		t.Fatal("explicit refresh left stale details or overwrote unsaved metadata")
	}
}

func TestBrowserUncertainTaskCreationSurvivesRefreshUntilNewRequest(t *testing.T) {
	var posts atomic.Int32
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/projects/demo/tasks" {
				posts.Add(1)
				result := httptest.NewRecorder()
				next.ServeHTTP(result, r)
				http.Error(w, "fixture acknowledgement lost", 503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	b, c := fixture.browser, fixture.browser.taskComposer
	c.createTitle.SetText("Unconfirmed task request")
	c.createSpec.SetText("Keep the handoff\nwhile its outcome is unknown")
	fynetest.Tap(c.createSubmit)
	draft := c.createDraft("demo")
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.inFlight && draft.state == taskCreateUnknown })
	b.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool { return !b.projectLoading && !activeTaskListLoading(b) && len(b.taskItems) == 3 })
	c.submitCreate()
	if posts.Load() != 1 || draft.state != taskCreateUnknown || c.createTitle.Text != "Unconfirmed task request" || !strings.Contains(c.createSpec.Text, "Keep the handoff") {
		t.Fatal("refresh hid or replayed the unconfirmed create")
	}
	fynetest.Tap(c.createSubmit) // This button now explicitly starts a new request.
	if draft.state != taskCreateReady || c.createTitle.Text != "" || c.createSpec.Text != "" || !c.createSubmit.Disabled() || posts.Load() != 1 {
		t.Fatal("new request did not clear the old operation without sending")
	}
}

func TestBrowserCreatedTaskReceiptStaysWithOriginalProject(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodPost, "/v1/projects/demo/tasks")
	fixture := startEditorBrowser(t, gate.wrap)
	b, c := fixture.browser, fixture.browser.taskComposer
	if err := fixture.store.InsertProject(context.Background(), storage.Project{Ident: "beta", ChannelName: "beta", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	c.createTitle.SetText("Create in demo")
	c.createSpec.SetText("Demo project specification")
	fynetest.Tap(c.createSubmit)
	gate.await(t)
	c.createTitle.SetText("Next unsent demo task")
	b.selectProject("beta")
	c.createTitle.SetText("Unsent beta task")
	gate.unblock()
	draft := c.createDraft("demo")
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.inFlight && draft.state == taskCreateAccepted })
	if b.selectedProject != "beta" || c.createTitle.Text != "Unsent beta task" || draft.createdTaskID == "" || gate.calls.Load() != 1 {
		t.Fatal("late accepted receipt changed the current project draft")
	}
	b.selectProject("demo")
	if c.createTitle.Text != "Next unsent demo task" || c.createSubmit.Text != "New task request" {
		t.Fatal("project navigation lost the newer draft or accepted receipt")
	}
	stored, err := fixture.store.Task(context.Background(), "demo", draft.createdTaskID)
	if err != nil || stored.Task.Title != "Create in demo" || stored.Task.OwnerOrigin != nil || stored.Task.Reporter != "user" {
		t.Fatalf("create changed payload or human provenance: %+v %v", stored, err)
	}
}

func TestBrowserCommentAckSurvivesAnOlderDetailCallback(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b, c := fixture.browser, fixture.browser.taskComposer
	oldGeneration := b.taskGen
	oldDetail := *b.selectedTaskDetail
	oldConversations := append([]storage.Conversation(nil), b.conversationItems...)
	c.commentEntry.SetText("Confirmed comment must remain visible")
	fynetest.Tap(c.commentSubmit)
	waitBrowser(t, fixture.callbacks, func() bool {
		return !c.commentDraft("demo", fixture.first.ID).inFlight && c.commentStatus.Text == "Comment posted."
	})
	// This is the same callback an already-running pre-POST detail GET would
	// dispatch. It must not erase the newer acknowledged comment.
	b.applyTaskDetail("demo", fixture.first.ID, oldGeneration, &oldDetail, oldConversations, "", false, nil)
	comments := b.taskComments.Objects
	if len(comments) != 1 || !strings.Contains(browserCardObject(comments[0].(*fyne.Container), 1).(*widget.Label).Text, "Confirmed comment must remain visible") || b.selectedTaskDetail.CommentCount != 1 {
		t.Fatal("old detail callback erased a confirmed comment")
	}
}

func TestBrowserCreateAckSurvivesAnOlderTaskListCallback(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b, c := fixture.browser, fixture.browser.taskComposer
	state := b.taskLists[b.activeWorkspaceID]
	oldWorkspace, oldBinding, oldQuery, oldStatus, oldAllStates := b.activeWorkspaceID, state.bindingGeneration, state.queryGeneration, state.status, state.allStates
	oldItems := append([]storage.TaskSummary(nil), b.taskItems...)
	c.createTitle.SetText("New task must remain discoverable")
	fynetest.Tap(c.createSubmit)
	waitBrowser(t, fixture.callbacks, func() bool { return c.createDraft("demo").state == taskCreateAccepted && len(b.taskItems) == 3 })
	created := c.createDraft("demo").createdTaskID
	b.applyWorkspaceTasks(oldWorkspace, "demo", oldBinding, oldQuery, oldStatus, oldAllStates, oldItems, nil)
	if !taskExists(b.taskItems, created) || b.selectedTask != fixture.first.ID {
		t.Fatal("old task-list callback erased the new task or changed selection")
	}
}

// A mutation's own acknowledgement must protect its UI even while the
// independent invalidation stream is unavailable or still reconnecting.
func withoutComposerNavigationStream(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/navigation/stream" {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestBrowserComposerLayout(t *testing.T) {
	fixture := startEditorBrowser(t, func(h http.Handler) http.Handler { return h })
	b, c := fixture.browser, fixture.browser.taskComposer
	c.createTitle.SetText("Review native conversation delivery")
	c.createSpec.SetText("Inspect the task-linked conversation.\nConfirm retries preserve one prompt and the current draft.")
	c.createGoal.SetText("desktop-parity")
	c.commentEntry.SetText("Checked the native worker reconnect.\nThe queued prompt and unsent draft remain intact.")
	window := fynetest.NewWindow(b.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	for _, title := range []string{"New task", "Task settings"} {
		accordion := findBrowserAccordion(b.Content(), title)
		if accordion == nil {
			t.Fatalf("missing accordion %q", title)
		}
		accordion.Open(0)
		if title != "New task" {
			if scroll, ok := accordion.Items[0].Detail.(*container.Scroll); ok {
				scroll.ScrollToBottom()
			}
		}
	}
	window.Resize(fyne.NewSize(1280, 820))
	b.Content().Refresh()
	file, err := os.Create(filepath.Join(t.TempDir(), "amux-task-composer-preview.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, window.Canvas().Capture()); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserSettledCreateReceiptIgnoresDuplicateAndResetCallbacks(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b, c := fixture.browser, fixture.browser.taskComposer
	c.createTitle.SetText("An accepted request settles once")
	fynetest.Tap(c.createSubmit)
	draft := c.createDraft("demo")
	waitBrowser(t, fixture.callbacks, func() bool { return draft.state == taskCreateAccepted && !draft.inFlight })
	operation, acceptedID := draft.operationGeneration, draft.createdTaskID
	detail, err := fixture.store.Task(context.Background(), "demo", acceptedID)
	if err != nil {
		t.Fatal(err)
	}
	c.finishCreate(b.activeWorkspaceID, "demo", operation, nil, errors.New("late failure for already accepted request"))
	if draft.state != taskCreateAccepted || draft.createdTaskID != acceptedID {
		t.Fatal("duplicate failure replaced the accepted receipt")
	}
	c.newTaskRequest()
	c.createTitle.SetText("A different unsent request")
	c.finishCreate(b.activeWorkspaceID, "demo", operation, &detail.Task, nil)
	if draft.state != taskCreateReady || draft.createdTaskID != "" || c.createTitle.Text != "A different unsent request" {
		t.Fatal("old callback settled an explicitly reset request")
	}
}
