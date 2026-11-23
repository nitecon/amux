package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func cancelWorkflowFixtureTask(t *testing.T, fixture editorBrowserFixture) {
	t.Helper()
	actor := &storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	if _, err := fixture.store.CancelTask(context.Background(), "demo", fixture.first.ID, "No longer needed", json.RawMessage(`null`), actor); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserWorkflowParkResumeAndClosePreserveOtherDrafts(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b, w := fixture.browser, fixture.browser.taskWorkflow
	b.taskEditor.titleEntry.SetText("Unsaved metadata title")
	b.taskComposer.commentEntry.SetText("Unsent comment while deciding")
	w.reviewAction.SetText("Review the proposed deletion before continuing")
	fynetest.Tap(w.park)
	waitBrowser(t, fixture.callbacks, func() bool { return !w.currentDraft().inFlight && b.selectedTaskDetail.Task.Status == "user_review" })
	if w.resume.Hidden || w.resume.Disabled() {
		t.Fatal("parked work cannot return to the queue")
	}
	fynetest.Tap(w.resume)
	waitBrowser(t, fixture.callbacks, func() bool { return !w.currentDraft().inFlight && b.selectedTaskDetail.Task.Status == "todo" })
	fynetest.Tap(w.close)
	waitBrowser(t, fixture.callbacks, func() bool { return !w.currentDraft().inFlight && b.selectedTaskDetail.Task.Status == "done" })
	if b.taskEditor.titleEntry.Text != "Unsaved metadata title" || b.taskComposer.commentEntry.Text != "Unsent comment while deciding" {
		t.Fatal("workflow action saved or discarded another draft")
	}
	stored, err := fixture.store.Task(context.Background(), "demo", fixture.first.ID)
	if err != nil || stored.Task.Title != "First" || stored.Task.Status != "done" || stored.Task.DoneAt == nil || stored.Task.OwnerOrigin != nil || stored.Task.ReviewAction != nil {
		t.Fatalf("unexpected human workflow effect: %+v %v", stored, err)
	}
}

func TestBrowserDelayedMetadataAckCannotUndoClosedTaskStatus(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodPatch, "")
	fixture := startEditorBrowser(t, func(h http.Handler) http.Handler { return gate.wrap(withoutComposerNavigationStream(h)) })
	gate.path = "/v1/projects/demo/tasks/" + fixture.first.ID
	b, w := fixture.browser, fixture.browser.taskWorkflow
	b.taskEditor.titleEntry.SetText("Explicitly saved metadata")
	fynetest.Tap(b.taskEditor.save)
	gate.await(t)
	fynetest.Tap(w.close)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail.Task.Status == "done" && !w.currentDraft().inFlight })
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return !b.taskEditor.drafts[taskEditorKey("demo", fixture.first.ID)].inFlight })
	if b.selectedTaskDetail.Task.Status != "done" || b.taskEditor.titleEntry.Text != "Explicitly saved metadata" {
		t.Fatal("older metadata acknowledgement undid the workflow result")
	}
	stored, err := fixture.store.Task(context.Background(), "demo", fixture.first.ID)
	if err != nil || stored.Task.Status != "done" || stored.Task.Title != "Explicitly saved metadata" {
		t.Fatalf("wrong combined mutation effect: %+v %v", stored, err)
	}
}

func TestBrowserUnknownWorkflowRefreshSettlesEvenAfterNavigation(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodGet, "")
	var writes atomic.Int32
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		api := withoutComposerNavigationStream(next)
		return gate.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				writes.Add(1)
				recorder := httptest.NewRecorder()
				api.ServeHTTP(recorder, r)
				http.Error(w, "fixture lost acknowledgement", 503)
				return
			}
			api.ServeHTTP(w, r)
		}))
	})
	b, w := fixture.browser, fixture.browser.taskWorkflow
	fynetest.Tap(w.close)
	draft := w.currentDraft()
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.inFlight && draft.unknown })
	w.submit("close")
	if writes.Load() != 1 {
		t.Fatal("uncertain workflow update was replayed")
	}
	gate.path = "/v1/projects/demo/tasks/" + fixture.first.ID
	fynetest.Tap(w.refresh)
	gate.await(t)
	b.selectTask(fixture.second.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID })
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.resolving })
	if b.selectedTask != fixture.second.ID {
		t.Fatal("offscreen refresh retargeted the current task")
	}
	b.selectTask(fixture.first.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID })
	if draft.resolving || draft.unknown || b.selectedTaskDetail.Task.Status != "done" || writes.Load() != 1 {
		t.Fatal("completed offscreen reconciliation stayed stuck or repeated the write")
	}
}

func TestBrowserCancellationReportIsReadOnlyAndDoesNotClaimExecutorStopped(t *testing.T) {
	var cancellationWrites atomic.Int32
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return withoutComposerNavigationStream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancellation") {
				cancellationWrites.Add(1)
			}
			next.ServeHTTP(w, r)
		}))
	})
	b, w := fixture.browser, fixture.browser.taskWorkflow
	cancelWorkflowFixtureTask(t, fixture)
	b.refreshTaskDetail()
	waitBrowser(t, fixture.callbacks, func() bool {
		return !w.cancellation.Hidden && strings.Contains(w.cancellation.Text, "No longer needed")
	})
	if !strings.Contains(strings.ToLower(w.cancellation.Text), "false") && !strings.Contains(strings.ToLower(w.cancellation.Text), "not confirmed") {
		t.Fatal("cancellation report implied the executor had stopped")
	}
	if cancellationWrites.Load() != 0 {
		t.Fatal("human report viewing sent an agent cancellation request")
	}
	b.selectTask(fixture.second.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID })
	if !w.cancellation.Hidden {
		t.Fatal("cancellation report leaked into another task")
	}
}

func TestBrowserUnknownWorkflowRemainsRecoverableAfterAnotherMutationRefresh(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodPatch, "")
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		next = withoutComposerNavigationStream(next)
		return gate.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				result := httptest.NewRecorder()
				next.ServeHTTP(result, r)
				http.Error(w, "fixture lost acknowledgement", 503)
				return
			}
			next.ServeHTTP(w, r)
		}))
	})
	gate.path = "/v1/projects/demo/tasks/" + fixture.first.ID
	b, w := fixture.browser, fixture.browser.taskWorkflow
	fynetest.Tap(w.close)
	gate.await(t)
	// A separate comment acknowledgement advances the detail read generation,
	// while the status writer still awaits its lost acknowledgement.
	b.taskComposer.commentEntry.SetText("Independent comment while the status reply is pending")
	fynetest.Tap(b.taskComposer.commentSubmit)
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.taskComposer.commentStatus.Text == "Comment posted." && !b.detailLoading && b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.Status == "done"
	})
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return !w.currentDraft().inFlight && w.currentDraft().unknown })
	if w.refresh.Hidden || w.refresh.Disabled() || !strings.Contains(w.status.Text, "unknown") {
		t.Fatal("another mutation refresh hid the unknown writer outcome and its recovery action")
	}
}

func TestBrowserCancellationReportRetriesAfterAnExplicitRefresh(t *testing.T) {
	var reads atomic.Int32
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return withoutComposerNavigationStream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cancellation") && reads.Add(1) == 1 {
				http.Error(w, "fixture temporarily offline", 503)
				return
			}
			next.ServeHTTP(w, r)
		}))
	})
	b, w := fixture.browser, fixture.browser.taskWorkflow
	cancelWorkflowFixtureTask(t, fixture)
	b.refreshTaskDetail()
	waitBrowser(t, fixture.callbacks, func() bool { return !w.cancelLoading && strings.Contains(w.cancellation.Text, "unavailable") })
	b.refreshTaskDetail()
	waitBrowser(t, fixture.callbacks, func() bool { return !b.detailLoading && !w.cancelLoading })
	if reads.Load() < 2 || !strings.Contains(w.cancellation.Text, "No longer needed") {
		t.Fatal("a transient report failure became permanent after refresh")
	}
}

func TestBrowserCreateReceiptRefreshesAfterAnotherWorkflowAction(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		next = withoutComposerNavigationStream(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/projects/demo/tasks" {
				close(arrived)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	b, c, w := fixture.browser, fixture.browser.taskComposer, fixture.browser.taskWorkflow
	c.createTitle.SetText("A new task committed after another workflow action")
	fynetest.Tap(c.createSubmit)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("create request did not arrive")
	}
	fynetest.Tap(w.close)
	waitBrowser(t, fixture.callbacks, func() bool {
		if b.selectedTaskDetail == nil || b.selectedTaskDetail.Task.Status != "done" || activeTaskListLoading(b) {
			return false
		}
		for _, row := range b.taskItems {
			if row.ID == fixture.first.ID {
				return row.Status == "done"
			}
		}
		return false
	})
	unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return c.createDraft("demo").state == taskCreateAccepted && !activeTaskListLoading(b) })
	if !taskExists(b.taskItems, c.createDraft("demo").createdTaskID) || b.selectedTask != fixture.first.ID {
		t.Fatal("an unrelated workflow refresh hid the accepted create or changed selection")
	}
}

func TestBrowserDisplaysRequestedReviewWithoutReplacingLocalDraft(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b, w := fixture.browser, fixture.browser.taskWorkflow
	w.reviewAction.SetText("A separate unsent local review draft")
	reason := "Review the proposed file deletion before returning this task to the queue."
	if _, err := fixture.store.UpdateTaskWorkflow(context.Background(), "demo", fixture.first.ID, storage.TaskWorkflowUpdate{Status: "user_review", ReviewAction: &reason, Policy: storage.TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
		t.Fatal(err)
	}
	b.refreshTaskDetail()
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail.Task.Status == "user_review" && !w.reviewPrompt.Hidden })
	if !strings.Contains(w.reviewPrompt.Text, reason) || w.reviewAction.Text != "A separate unsent local review draft" {
		t.Fatal("requested review was hidden or replaced a local draft")
	}
	fynetest.Tap(w.resume)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail.Task.Status == "todo" })
	if !w.reviewPrompt.Hidden || w.reviewAction.Text != "A separate unsent local review draft" {
		t.Fatal("resolved review leaked into the queue or lost the local draft")
	}
}

func TestBrowserBoardMovesSubtaskAndOpensItsDetails(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b := fixture.browser
	ctx := context.Background()
	child, err := fixture.store.InsertTask(ctx, storage.TaskInsert{
		Project: "demo", Title: "Review first task", ParentID: &fixture.first.ID, Reporter: "fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.refreshTaskDetail()
	waitBrowser(t, fixture.callbacks, func() bool {
		objects := b.taskSubtasks.Objects
		return len(objects) == 2 && objects[1].(*widget.Button).Text == "Review first task · Queued" &&
			strings.Contains(b.taskMeta.Text, " · general")
	})
	window := fynetest.NewWindow(b.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	captureHUDState(t, "task-hierarchy", window.Canvas())
	hasChild := func(lane *taskBoardLane) bool {
		for _, task := range lane.items {
			if task.ID == child.ID && task.Parent != nil && task.Parent.ID == fixture.first.ID {
				return true
			}
		}
		return false
	}
	b.setMainView("Board")
	waitBrowser(t, fixture.callbacks, func() bool { return hasChild(b.taskBoard.lanes[0]) })
	actor := &storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	if _, err := fixture.store.ApplyTaskTransition(ctx, "demo", child.ID, "in_progress", actor, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	b.taskBoard.refresh()
	waitBrowser(t, fixture.callbacks, func() bool {
		active := b.taskBoard.lanes[0].items
		return len(active) > 1 && active[0].ID == child.ID &&
			active[0].Status == "in_progress" && active[1].Status == "todo"
	})
	reason := "Check the Board lane for parked work."
	if _, err := fixture.store.UpdateTaskWorkflow(ctx, "demo", child.ID, storage.TaskWorkflowUpdate{
		Status: "user_review", ReviewAction: &reason, Policy: storage.TaskTransitionPolicy{HumanRequest: true},
	}, nil); err != nil {
		t.Fatal(err)
	}
	b.taskBoard.refresh()
	waitBrowser(t, fixture.callbacks, func() bool {
		return len(b.taskBoard.lanes) == 3 && !hasChild(b.taskBoard.lanes[0]) && hasChild(b.taskBoard.lanes[1])
	})
	b.taskBoard.lanes[1].list.Select(0)
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.mainView == "Tasks" && b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == child.ID
	})
}

func TestBrowserRestoresConversationViewAndSelection(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := &storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	chat, err := fixture.store.CreateConversation(ctx, "demo", "", owner)
	if err != nil {
		t.Fatal(err)
	}
	prefsPath := filepath.Join(t.TempDir(), "navigation.json")
	prefs := NavigationPreferences{
		Favorites: map[string]bool{},
		Selections: map[string]NavigationSelection{fixture.browser.endpoint: {
			Project: "demo", Conversation: chat.ID, Status: browserStatusActive, MainView: "Conversations",
		}},
	}
	if err := SaveNavigationPreferences(prefsPath, prefs); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	b, err := NewBrowser(BrowserConfig{Client: fixture.browser.client, PreferencesPath: prefsPath, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { b.Close(); b.Wait(); callbacks.pump() }()
	b.Start(ctx)
	waitBrowser(t, callbacks, func() bool {
		return b.mainView == "Conversations" && b.selectedConversation == chat.ID &&
			len(b.standaloneChatHost.Objects) == 1 && b.prefs.Selections[workspaceSelectionKey(b.prefKey, b.activeWorkspaceID)].Conversation == chat.ID
	})
}

func TestTaskBoardCardDragMovesTaskAcrossLanes(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b := fixture.browser
	window := fynetest.NewWindow(b.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	b.setMainView("Board")
	waitBrowser(t, fixture.callbacks, func() bool { return len(b.taskBoard.lanes[0].items) > 0 })
	target := b.taskBoard.lanes[2].list
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(target)
	size := target.Size()
	dropAt := fyne.NewPos(pos.X+size.Width/2, pos.Y+size.Height/2)
	card := newTaskBoardCard(b.taskBoard)
	card.task = storage.TaskSummary{ID: fixture.first.ID, Status: "todo"}
	card.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{AbsolutePosition: dropAt}})
	card.DragEnd()
	waitBrowser(t, fixture.callbacks, func() bool {
		rows, err := fixture.store.Tasks(context.Background(), "demo", []string{"done"}, false)
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.ID == fixture.first.ID {
				return true
			}
		}
		return false
	})
}

func TestTaskBoardCardCanReorderWithinUserReview(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b := fixture.browser
	ctx := context.Background()
	action := "Review this result."
	for _, id := range []string{fixture.first.ID, fixture.second.ID} {
		if _, err := fixture.store.UpdateTaskWorkflow(ctx, "demo", id, storage.TaskWorkflowUpdate{
			Status: "user_review", ReviewAction: &action, Policy: storage.TaskTransitionPolicy{HumanRequest: true},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	window := fynetest.NewWindow(b.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	b.setMainView("Board")
	waitBrowser(t, fixture.callbacks, func() bool {
		return len(b.taskBoard.lanes[1].items) == 2 && b.taskBoard.lanes[1].cards[fixture.first.ID] != nil
	})
	target := b.taskBoard.lanes[1].cards[fixture.first.ID]
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(target)
	dropAt := fyne.NewPos(pos.X+target.Size().Width/2, pos.Y+1)
	card := newTaskBoardCard(b.taskBoard)
	card.task = storage.TaskSummary{ID: fixture.second.ID, Status: "user_review"}
	card.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{AbsolutePosition: dropAt}})
	card.DragEnd()
	waitBrowser(t, fixture.callbacks, func() bool {
		rows, err := fixture.store.Tasks(ctx, "demo", []string{"user_review"}, false)
		return err == nil && len(rows) == 2 && rows[0].ID == fixture.second.ID &&
			len(b.taskBoard.lanes[1].items) == 2 && b.taskBoard.lanes[1].items[0].ID == fixture.second.ID
	})
}

func TestTaskBoardShowsDoneReorderFromAcknowledgement(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	ctx := context.Background()
	board := fixture.browser.taskBoard
	window := fynetest.NewWindow(fixture.browser.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	fixture.browser.setMainView("Board")
	waitBrowser(t, fixture.callbacks, func() bool { return !board.loading && len(board.lanes[0].items) > 0 })
	for _, id := range []string{fixture.first.ID, fixture.second.ID} {
		if _, err := fixture.store.UpdateTaskWorkflow(ctx, "demo", id, storage.TaskWorkflowUpdate{
			Status: "done", Policy: storage.TaskTransitionPolicy{HumanRequest: true},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	items, err := fixture.store.Tasks(ctx, "demo", browserTaskStatuses(browserStatusAll), false)
	if err != nil {
		t.Fatal(err)
	}
	board.setItems(items)
	waitBrowser(t, fixture.callbacks, func() bool { return board.lanes[2].cards[fixture.first.ID] != nil })
	target := board.lanes[2].cards[fixture.first.ID]
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(target)
	dropAt := fyne.NewPos(pos.X+target.Size().Width/2, pos.Y+1)
	card := newTaskBoardCard(board)
	card.task = storage.TaskSummary{ID: fixture.second.ID, Status: "done"}
	card.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{AbsolutePosition: dropAt}})
	card.DragEnd()
	waitBrowser(t, fixture.callbacks, func() bool {
		return len(board.lanes[2].items) == 2 && board.lanes[2].items[0].ID == fixture.second.ID && board.lanes[2].items[1].ID == fixture.first.ID
	})
}

func TestBrowserReviewNoticeRecoversWithoutNavigationEventAndDoesNotRepeat(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b := fixture.browser
	waitBrowser(t, fixture.callbacks, func() bool {
		_, initialized := b.reviewNoticeCursors["demo"]
		return initialized
	})
	action := "Review and accept this result."
	if _, err := fixture.store.UpdateTaskWorkflow(context.Background(), "demo", fixture.first.ID, storage.TaskWorkflowUpdate{
		Status: "user_review", ReviewAction: &action, Policy: storage.TaskTransitionPolicy{HumanRequest: true},
	}, nil); err != nil {
		t.Fatal(err)
	}
	refresh := func() {
		b.refreshReviewNotices("demo")
		waitBrowser(t, fixture.callbacks, func() bool { return !b.reviewNoticesLoading })
	}
	fynetest.AssertNotificationSent(t, fyne.NewNotification("Task ready for review", "demo · First"), refresh)
	fynetest.AssertNotificationSent(t, nil, refresh)
	b.refreshTaskDetail()
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.selectedTaskDetail.Task.Status == "user_review"
	})
	fynetest.Tap(b.taskWorkflow.close)
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.selectedTaskDetail.Task.Status == "done"
	})
}

func TestBrowserWorkflowLayout(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b := fixture.browser
	reason := "Review the proposed file deletion before returning this task to the queue."
	if _, err := fixture.store.UpdateTaskWorkflow(context.Background(), "demo", fixture.first.ID, storage.TaskWorkflowUpdate{Status: "user_review", ReviewAction: &reason, Policy: storage.TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
		t.Fatal(err)
	}
	b.refreshTaskDetail()
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail.Task.Status == "user_review" })
	b.refreshTasks()
	waitBrowser(t, fixture.callbacks, func() bool {
		for _, task := range b.visibleTasks {
			if task.ID == fixture.first.ID {
				return task.Status == "user_review"
			}
		}
		return false
	})
	window := fynetest.NewWindow(b.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	accordion := findBrowserAccordion(b.Content(), "Task settings")
	if accordion == nil {
		t.Fatal("missing task details accordion")
	}
	b.Content().Refresh()
	captureHUDState(t, "user-review", window.Canvas())
	if err := writeHUDPNG(window.Canvas(), filepath.Join(t.TempDir(), "amux-task-workflow-preview.png")); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserWorkflowReceiptCannotRetargetAnotherProject(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodPatch, "")
	fixture := startEditorBrowser(t, func(h http.Handler) http.Handler { return gate.wrap(withoutComposerNavigationStream(h)) })
	b, w := fixture.browser, fixture.browser.taskWorkflow
	gate.path = "/v1/projects/demo/tasks/" + fixture.first.ID
	if err := fixture.store.InsertProject(context.Background(), storage.Project{Ident: "beta", ChannelName: "beta", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	other, err := fixture.store.InsertTask(context.Background(), storage.TaskInsert{Project: "beta", Title: "Another project", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fynetest.Tap(w.close)
	gate.await(t)
	b.selectProject("beta")
	waitBrowser(t, fixture.callbacks, func() bool { return len(b.taskItems) == 1 })
	b.selectTask(other.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == other.ID })
	w.reviewAction.SetText("Unsent beta decision")
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return !w.drafts[taskEditorKey("demo", fixture.first.ID)].inFlight })
	if b.selectedProject != "beta" || b.selectedTask != other.ID || b.selectedTaskDetail.Task.Status != "todo" || w.reviewAction.Text != "Unsent beta decision" {
		t.Fatal("late workflow acknowledgement retargeted another project")
	}
}
