package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func TestTaskMetadataDiffSendsOnlyChangedFieldsAndMapsEmptyToNull(t *testing.T) {
	base := storage.Task{ID: "one", ProjectIdent: "demo", Title: "Original", Goal: stringPtr(" keep whitespace "), Description: stringPtr("old"), Details: stringPtr("spec"), Hostname: stringPtr("host"), Labels: []string{"comma,label", "café", "雪"}, Rank: 7}
	draft := taskEditorValue(base)
	draft.title = "Typed title"
	update, err := taskMetadataDiff(base, draft)
	if err != nil {
		t.Fatal(err)
	}
	if update.Title == nil || *update.Title != "Typed title" || update.Specification != nil || update.Description != nil || update.Labels != nil {
		t.Fatalf("unexpected field diff: %+v", update)
	}

	draft.description = ""
	draft.labels = encodeTaskLabels([]string{"comma,label", "café", "雪", `path\name`})
	update, err = taskMetadataDiff(base, draft)
	if err != nil {
		t.Fatal(err)
	}
	if string(update.Description) != "null" {
		t.Fatalf("cleared description = %s", update.Description)
	}
	var got []string
	if err := json.Unmarshal(update.Labels, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"comma,label", "café", "雪", `path\name`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("labels = %#v, want %#v", got, want)
	}
}

func TestTaskEditorRefreshAndNavigationKeepOnlyDirtyValues(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	browser, err := NewBrowser(BrowserConfig{Client: inertEditorClient(t)})
	if err != nil {
		t.Fatal(err)
	}
	e := browser.taskEditor
	browser.selectedProject, browser.selectedTask = "demo", "one"
	e.selectTask("demo", "one")
	first := storage.Task{ID: "one", ProjectIdent: "demo", Title: "Original", Description: stringPtr("before"), Details: stringPtr("old spec"), Rank: 3}
	e.setAuthoritative("demo", "one", first)
	e.titleEntry.SetText("My draft")

	updated := first
	updated.Description = stringPtr("external description")
	updated.Details = stringPtr("external specification")
	updated.Rank = 9
	e.setAuthoritative("demo", "one", updated)
	e.selectTask("demo", "other")
	e.setAuthoritative("demo", "other", storage.Task{ID: "other", ProjectIdent: "demo", Title: "Other"})
	e.selectTask("demo", "one")
	draft := e.drafts[taskEditorKey("demo", "one")]
	if draft.title != "My draft" || draft.description != "external description" || draft.specification != "external specification" || draft.rank != "9" {
		t.Fatalf("draft after refresh/navigation = %+v", draft)
	}
	update, err := taskMetadataDiff(draft.base, *draft)
	if err != nil {
		t.Fatal(err)
	}
	if update.Title == nil || update.Description != nil || update.Specification != nil || update.Rank != nil {
		t.Fatalf("refresh turned untouched fields into changes: %+v", update)
	}
	e.cancelDraft()
	if e.drafts[e.selectedKey].title != "Original" {
		t.Fatal("Cancel did not reset to latest authoritative value")
	}
}

func TestTaskEditorLabelsWithExistingNewlineStayUntouchedOnOtherEdits(t *testing.T) {
	base := storage.Task{Title: "old", Labels: []string{"ordinary", "multi\nline"}}
	draft := taskEditorValue(base)
	draft.title = "new"
	update, err := taskMetadataDiff(base, draft)
	if err != nil || update.Title == nil || update.Labels != nil {
		t.Fatalf("unrelated edit: %+v, %v", update, err)
	}
	draft.labels += "\nextra"
	if _, err := taskMetadataDiff(base, draft); err == nil {
		t.Fatal("editing ambiguous newline-bearing labels should be rejected")
	}
}

func TestTaskEditorInvalidRankShowsInlineErrorAndEmptyDiffDisablesSave(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	browser, err := NewBrowser(BrowserConfig{Client: inertEditorClient(t)})
	if err != nil {
		t.Fatal(err)
	}
	e := browser.taskEditor
	e.selectTask("demo", "one")
	e.setAuthoritative("demo", "one", storage.Task{ID: "one", ProjectIdent: "demo", Title: "same"})
	if !e.save.Disabled() {
		t.Fatal("Save enabled with an empty diff")
	}
	e.rankEntry.SetText("1.5")
	if !e.save.Disabled() || !strings.Contains(e.status.Text, "whole number") {
		t.Fatalf("invalid rank feedback: saveEnabled=%v status=%q", !e.save.Disabled(), e.status.Text)
	}
}

func TestTaskEditorFailedAndLateSaveKeepDraftBoundToTask(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	started, release := make(chan struct{}, 1), make(chan struct{})
	var writes atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		writes.Add(1)
		var update storage.TaskMetadataUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode PATCH: %v", err)
		}
		if update.Title == nil || update.Description != nil || update.Specification != nil {
			t.Errorf("unexpected PATCH fields: %+v", update)
		}
		started <- struct{}{}
		<-release
		_ = json.NewEncoder(w).Encode(storage.Task{ID: "one", ProjectIdent: "demo", Title: "Saved title", Description: stringPtr("latest from server"), Details: stringPtr("latest spec"), Rank: 8})
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	browser.ctx = context.Background()
	browser.selectedProject, browser.selectedTask = "demo", "one"
	e := browser.taskEditor
	e.selectTask("demo", "one")
	e.setAuthoritative("demo", "one", storage.Task{ID: "one", ProjectIdent: "demo", Title: "Old title", Description: stringPtr("old"), Details: stringPtr("old spec"), Rank: 1})
	e.titleEntry.SetText("Saved title")
	e.saveTaskDraft()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("PATCH did not start")
	}
	if e.drafts[e.selectedKey].inFlight == false {
		t.Fatal("save was not marked in flight")
	}
	newer := e.drafts[taskEditorKey("demo", "one")].base
	newer.Description, newer.Details, newer.Rank = stringPtr("external description"), stringPtr("external spec"), 8
	e.setAuthoritative("demo", "one", newer)

	// Navigate while the immutable save request is outstanding.
	browser.selectedTask = "two"
	e.selectTask("demo", "two")
	e.setAuthoritative("demo", "two", storage.Task{ID: "two", ProjectIdent: "demo", Title: "Other task"})
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		callbacks.pump()
		if !e.drafts[taskEditorKey("demo", "one")].inFlight {
			break
		}
		time.Sleep(time.Millisecond)
	}
	callbacks.pump()
	if e.titleEntry.Text != "Other task" {
		t.Fatalf("late response corrupted current task form: %q", e.titleEntry.Text)
	}
	lateDraft := e.drafts[taskEditorKey("demo", "one")]
	if lateDraft.base.Description == nil || *lateDraft.base.Description != "external description" || lateDraft.base.Details == nil || *lateDraft.base.Details != "external spec" {
		t.Fatal("late save replaced unrelated fields from a newer detail")
	}
	if writes.Load() != 1 {
		t.Fatalf("write count %d, want one", writes.Load())
	}
	browser.Close()
	browser.Wait()
}

func TestTaskEditorFailureRetainsDraftAndDisplaysHTTPStatus(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	var writes atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		http.Error(w, "private response detail", http.StatusBadGateway)
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	browser.ctx = context.Background()
	browser.selectedProject, browser.selectedTask = "demo", "one"
	e := browser.taskEditor
	e.selectTask("demo", "one")
	e.setAuthoritative("demo", "one", storage.Task{ID: "one", ProjectIdent: "demo", Title: "Original"})
	e.titleEntry.SetText("Draft")
	e.saveTaskDraft()
	waitEditorUI(t, callbacks, func() bool { return !e.drafts[e.selectedKey].inFlight })
	if e.drafts[e.selectedKey].title != "Draft" || !strings.Contains(e.status.Text, "HTTP 502") || strings.Contains(e.status.Text, "private response detail") {
		t.Fatalf("failed save feedback/draft: title=%q status=%q", e.drafts[e.selectedKey].title, e.status.Text)
	}
	if writes.Load() != 1 {
		t.Fatalf("failed write retried: %d requests", writes.Load())
	}
	browser.Close()
	browser.Wait()
}

func TestBrowserCloseCancelsAndDrainsBlockedEditorSave(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	started := make(chan struct{}, 1)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: func(func()) {}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	browser.ctx, browser.cancel = ctx, cancel
	browser.selectedProject, browser.selectedTask = "demo", "one"
	e := browser.taskEditor
	e.selectTask("demo", "one")
	e.setAuthoritative("demo", "one", storage.Task{ID: "one", ProjectIdent: "demo", Title: "Original"})
	e.titleEntry.SetText("Draft")
	e.saveTaskDraft()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("PATCH did not start")
	}
	browser.Close()
	done := make(chan struct{})
	go func() { browser.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Browser.Wait did not drain canceled editor save")
	}
	cancel()
}

func waitEditorUI(t *testing.T, callbacks *uiCallbacks, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		callbacks.pump()
		if done() {
			callbacks.pump()
			return
		}
		time.Sleep(time.Millisecond)
	}
	callbacks.pump()
	if !done() {
		t.Fatal("timed out waiting for editor callback")
	}
}

func inertEditorClient(t *testing.T) *gatewayclient.Client {
	t.Helper()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(host.Close)
	client, err := gatewayclient.New(host.URL, "test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func stringPtr(value string) *string { return &value }
