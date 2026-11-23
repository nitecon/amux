package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/storage"
)

func browserPluginControlsReady(p *pluginSettingsPanel) bool {
	if len(p.checks) != len(storage.SwitchablePluginIDs()) {
		return false
	}
	for _, id := range storage.SwitchablePluginIDs() {
		if p.checks[id] == nil || p.checks[id].Disabled() {
			return false
		}
	}
	return true
}

func loadBrowserPluginControls(t *testing.T, fixture editorBrowserFixture) *pluginSettingsPanel {
	t.Helper()
	p := fixture.browser.pluginSettings
	p.accordion.Open(0)
	if !p.refresh.Disabled() {
		fynetest.Tap(p.refresh)
	}
	waitBrowser(t, fixture.callbacks, func() bool { return browserPluginControlsReady(p) })
	return p
}

func TestBrowserPluginSettingReceiptPreservesTaskDraftsAndDoesNotEmitLoadWrites(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodPut, "/v1/plugins/review")
	fixture := startEditorBrowser(t, gate.wrap)
	p := loadBrowserPluginControls(t, fixture)
	if gate.calls.Load() != 0 {
		t.Fatal("loading settings emitted a write")
	}
	b := fixture.browser
	b.taskEditor.titleEntry.SetText("Unsaved title while configuring the server")
	b.taskComposer.commentEntry.SetText("Unsent comment while configuring the server")
	fynetest.Tap(p.checks["review"])
	gate.await(t)
	stored, err := fixture.store.PluginEnabled(context.Background(), "review")
	if err != nil || stored {
		t.Fatalf("server did not commit the requested disable: %v %v", stored, err)
	}
	if !p.checks["review"].Disabled() {
		t.Fatal("pending setting permits another mutation")
	}
	fynetest.Tap(p.checks["review"])
	b.selectTask(fixture.second.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID })
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return browserPluginControlsReady(p) && !p.checks["review"].Checked })
	if b.selectedTask != fixture.second.ID || gate.calls.Load() != 1 {
		t.Fatal("server setting receipt retargeted a task or repeated its write")
	}
	b.selectTask(fixture.first.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID })
	if b.taskEditor.titleEntry.Text != "Unsaved title while configuring the server" || b.taskComposer.commentEntry.Text != "Unsent comment while configuring the server" {
		t.Fatal("server settings changed task drafts")
	}
	fynetest.Tap(p.refresh)
	waitBrowser(t, fixture.callbacks, func() bool { return browserPluginControlsReady(p) })
	if p.checks["review"].Checked || gate.calls.Load() != 1 {
		t.Fatal("explicit load lost the acknowledged state or emitted a write")
	}
}

func TestBrowserPluginSettingBadAcknowledgementRequiresSuccessfulExplicitRefresh(t *testing.T) {
	var puts atomic.Int32
	var failLoad atomic.Bool
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/plugins" && failLoad.Load() {
				http.Error(w, "synthetic unavailable settings", http.StatusServiceUnavailable)
				return
			}
			if r.Method == http.MethodPut && r.URL.Path == "/v1/plugins/review" {
				puts.Add(1)
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"notifier","enabled":false}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	p := loadBrowserPluginControls(t, fixture)
	fynetest.Tap(p.checks["review"])
	waitBrowser(t, fixture.callbacks, func() bool {
		return !p.refresh.Disabled() && !browserPluginControlsReady(p) && strings.TrimSpace(p.status.Text) != ""
	})
	fynetest.Tap(p.checks["review"])
	if puts.Load() != 1 {
		t.Fatal("unconfirmed plugin update was replayed")
	}
	fixture.browser.refreshOnUI()
	waitBrowser(t, fixture.callbacks, func() bool { return !fixture.browser.projectLoading })
	if browserPluginControlsReady(p) || puts.Load() != 1 {
		t.Fatal("background navigation refresh cleared plugin write uncertainty")
	}
	failLoad.Store(true)
	fynetest.Tap(p.refresh)
	waitBrowser(t, fixture.callbacks, func() bool { return !p.refresh.Disabled() })
	if browserPluginControlsReady(p) {
		t.Fatal("failed reload cleared write uncertainty")
	}
	failLoad.Store(false)
	fynetest.Tap(p.refresh)
	waitBrowser(t, fixture.callbacks, func() bool { return browserPluginControlsReady(p) })
	if p.checks["review"].Checked || puts.Load() != 1 {
		t.Fatal("reconciliation missed the committed setting or repeated the write")
	}
}
