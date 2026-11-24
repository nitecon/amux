package desktop

import (
	"context"
	"errors"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

func TestPluginSettingsPanelUsesServerValuesInSwitchableOrder(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	b := &Browser{browserStatus: widget.NewLabel("Ready")}
	p := newPluginSettingsPanel(b)
	p.readGeneration = 1
	ids := storage.SwitchablePluginIDs()
	settings := make([]storage.PluginSetting, len(ids))
	for i, id := range ids {
		settings[i] = storage.PluginSetting{ID: id, Enabled: i%2 == 0}
	}
	p.finishReload(1, 0, settings, nil)
	for i, id := range ids {
		check := p.checks[id]
		if check == nil || check.Text != pluginSettingLabels[id] || check.Checked != (i%2 == 0) || check.Disabled() {
			t.Fatalf("plugin %q did not use the ordered server value and friendly label", id)
		}
	}
	if !p.loaded || p.uncertain {
		t.Fatal("valid server settings response did not unlock the panel")
	}
}

func TestPluginSettingsPanelRejectsIncompleteOrMisorderedRead(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	b := &Browser{browserStatus: widget.NewLabel("Ready")}
	p := newPluginSettingsPanel(b)
	p.readGeneration = 1
	ids := storage.SwitchablePluginIDs()
	settings := make([]storage.PluginSetting, len(ids))
	for i, id := range ids {
		settings[i] = storage.PluginSetting{ID: id, Enabled: true}
	}
	settings[0], settings[1] = settings[1], settings[0]
	p.finishReload(1, 0, settings, nil)
	if p.loaded || !p.uncertain || p.checks[ids[0]].Checked || !p.checks[ids[0]].Disabled() {
		t.Fatal("misordered server settings were treated as confirmed values")
	}
}

func TestPluginSettingsAcceptOnlyCurrentServerResponses(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	b := &Browser{browserStatus: widget.NewLabel("Ready")}
	p := newPluginSettingsPanel(b)
	ids := storage.SwitchablePluginIDs()
	for _, id := range ids {
		p.confirmed[id] = true
	}
	p.loaded = true
	p.programmatic = true
	p.checks["review"].SetChecked(true)
	p.programmatic = false
	p.updateControls()

	staleRead := make([]storage.PluginSetting, len(ids))
	for i, id := range ids {
		staleRead[i] = storage.PluginSetting{ID: id, Enabled: false}
	}
	p.readGeneration = 2
	p.writeGeneration = 3
	p.writing = true // a newer toggle is still awaiting its acknowledgement
	p.finishReload(1, 2, staleRead, nil)
	p.finishWrite(2, "review", false, &storage.PluginSetting{ID: "review", Enabled: false}, nil)
	if !p.writing || p.uncertain || !p.confirmed["review"] || !p.checks["review"].Checked {
		t.Fatal("stale read or write changed a newer pending server value")
	}

	p.finishWrite(3, "review", false, &storage.PluginSetting{ID: "review", Enabled: false}, nil)
	if p.writing || p.uncertain || p.confirmed["review"] || p.checks["review"].Checked || p.checks["review"].Disabled() {
		t.Fatal("matching acknowledgement did not confirm the requested false value")
	}

	p.readGeneration = 4 // a GET already in flight before the next PUT
	p.writeGeneration = 4
	p.writing = true
	p.finishWrite(4, "review", true, nil, errors.New("server unavailable"))
	p.finishReload(4, 3, staleRead, nil)
	if p.writing || !p.uncertain || p.confirmed["review"] || p.checks["review"].Checked || !p.checks["review"].Disabled() || !strings.Contains(p.status.Text, "server unavailable") {
		t.Fatal("current write failure did not become unknown or stale GET cleared uncertainty")
	}
}

func TestPluginSettingsQueuedRefreshCannotMutateAfterBrowserClose(t *testing.T) {
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler { return handler })
	b, p := fixture.browser, fixture.browser.pluginSettings
	waitBrowser(t, fixture.callbacks, func() bool { return p.loaded })
	readGeneration, writeGeneration := p.readGeneration, p.writeGeneration
	status, review := p.status.Text, p.checks["review"].Checked
	b.Refresh() // leave the refresh callback queued on the UI dispatcher
	b.Close()   // closing is synchronous; its widget callback is queued too
	p.checks["review"].SetChecked(!review)
	fixture.callbacks.pump()
	stored, err := fixture.store.PluginEnabled(context.Background(), "review")
	if err != nil || stored != review {
		t.Fatalf("queued post-close action changed the server setting: enabled=%v err=%v", stored, err)
	}
	if p.readGeneration != readGeneration || p.writeGeneration != writeGeneration || p.status.Text != status || p.checks["review"].Checked != review || !p.closed {
		t.Fatal("queued refresh or check callback changed panel state after browser close")
	}
}

func TestBrowserPluginSettingsWrappedStatusFitsProjectColumn(t *testing.T) {
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler { return handler })
	b, p := fixture.browser, fixture.browser.pluginSettings
	waitBrowser(t, fixture.callbacks, func() bool { return p.loaded })
	window := fynetest.NewWindow(b.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))
	window.Show()
	openBrowserSettingsPage(t, b, "Automation")
	p.accordion.Open(0)
	p.loaded = true
	p.confirmed["review"] = false
	p.writeGeneration++
	p.writing = true
	p.finishWrite(p.writeGeneration, "review", true, nil, errors.New(strings.Repeat("server unavailable ", 8)))
	_ = window.Canvas().Capture()
	columnWidth := b.mainViews["Settings"].Size().Width
	if p.status.Size().Height+1 < p.status.MinSize().Height {
		t.Fatalf("wrapped status allocated %v height, needs %v", p.status.Size().Height, p.status.MinSize().Height)
	}
	if p.status.Size().Width > columnWidth+1 {
		t.Fatalf("status width %v exceeds project column %v", p.status.Size().Width, columnWidth)
	}
	for id, check := range p.checks {
		if check.MinSize().Width > columnWidth+1 {
			t.Fatalf("plugin control %q needs %v width in %v project column", id, check.MinSize().Width, columnWidth)
		}
	}
	preview, err := os.Create(filepath.Join(t.TempDir(), "amux-plugin-settings-preview.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(preview, window.Canvas().Capture()); err != nil {
		preview.Close()
		t.Fatal(err)
	}
	if err := preview.Close(); err != nil {
		t.Fatal(err)
	}
}
