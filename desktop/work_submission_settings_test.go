package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/worksubmission"
)

func TestWorkSubmissionSettingsDraftSavesOnlyOnSaveAndCancelRestoresLastSaved(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	path := filepath.Join(t.TempDir(), "nested", "work-preferences.json")
	p := newWorkSubmissionSettingsPanel(path, nil)
	window := fynetest.NewWindow(p.accordion)
	defer window.Close()
	window.Show()
	p.accordion.Open(0)
	if p.draft != worksubmission.DefaultPreferences() {
		t.Fatalf("initial draft = %+v", p.draft)
	}
	p.mode.SetSelected("Pull request")
	p.prefix.SetText("review/")
	want := worksubmission.Preferences{Mode: worksubmission.PullRequestMode, BranchPrefix: "review/"}
	if p.draft != want {
		t.Fatalf("draft edits = %+v, want %+v", p.draft, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("editing draft wrote settings before Save: %v", err)
	}
	p.save.OnTapped()
	if got, err := worksubmission.LoadPreferences(path); err != nil || got != want {
		t.Fatalf("saved settings = %+v, %v", got, err)
	}
	p.prefix.SetText("discarded/")
	p.cancel.OnTapped()
	if p.draft != want || p.prefix.Text != want.BranchPrefix || !strings.Contains(p.status.Text, "discarded") {
		t.Fatalf("Cancel did not restore the last saved draft: %+v status=%q", p.draft, p.status.Text)
	}
	if got, err := worksubmission.LoadPreferences(path); err != nil || got != want {
		t.Fatalf("Cancel changed saved preferences: %+v, %v", got, err)
	}
}

func TestWorkSubmissionSettingsSaveErrorKeepsDraftAndCancelKeepsLoadErrorVisible(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	const invalid = `{"work_submission":{"mode":null}}`
	if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
		t.Fatal(err)
	}
	p := newWorkSubmissionSettingsPanel(path, nil)
	window := fynetest.NewWindow(p.accordion)
	defer window.Close()
	window.Show()
	p.accordion.Open(0)
	p.mode.SetSelected("Pull request")
	p.prefix.SetText("review/")
	wantDraft := worksubmission.Preferences{Mode: worksubmission.PullRequestMode, BranchPrefix: "review/"}
	p.save.OnTapped()
	if p.draft != wantDraft || !strings.Contains(p.status.Text, "Could not save work submission preferences") {
		t.Fatalf("failed Save lost draft or error: %+v status=%q", p.draft, p.status.Text)
	}
	p.cancel.OnTapped()
	if p.draft != worksubmission.DefaultPreferences() || !strings.Contains(p.status.Text, "Could not load work submission preferences") {
		t.Fatalf("Cancel did not restore defaults and retain load error: %+v status=%q", p.draft, p.status.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != invalid {
		t.Fatalf("failed settings save overwrote invalid file: %q, %v", data, err)
	}
}

func TestWorkSubmissionSettingsWrappedStatusFitsTaskColumn(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	p := newWorkSubmissionSettingsPanel(filepath.Join(t.TempDir(), "prefs.json"), nil)
	root := container.NewHSplit(p.accordion, widget.NewLabel("Details"))
	p.setRoot(root)
	window := fynetest.NewWindow(root)
	defer window.Close()
	window.Resize(fyne.NewSize(1120, 760))
	window.Show()
	p.accordion.Open(0)
	p.setStatus("Could not save work submission preferences: " + strings.Repeat("synthetic path and permission error ", 8))
	_ = window.Canvas().Capture()
	if p.status.Size().Height+1 < p.status.MinSize().Height {
		t.Fatalf("wrapped status size %v at %v is smaller than its minimum %v; accordion=%v open=%v root=%v prefix=%v mode=%v", p.status.Size(), p.status.Position(), p.status.MinSize(), p.accordion.Size(), p.accordion.Items[0].Open, root.Size(), p.prefix.Size(), p.mode.Size())
	}
	if p.status.Size().Width > p.accordion.Size().Width+1 {
		t.Fatalf("status width %v exceeds task column %v", p.status.Size().Width, p.accordion.Size().Width)
	}
}

func TestWorkSubmissionSettingsIgnoresDirectSaveAfterClose(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	path := filepath.Join(t.TempDir(), "prefs.json")
	p := newWorkSubmissionSettingsPanel(path, nil)
	p.draft.Mode = worksubmission.PullRequestMode
	p.close()
	p.saveDraft()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("closed panel persisted settings: %v", err)
	}
}

func TestWorkSubmissionSettingsStaleDraftKeepsFieldChangedElsewhere(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	p := newWorkSubmissionSettingsPanel(path, nil)
	window := fynetest.NewWindow(p.accordion)
	defer window.Close()
	window.Show()
	p.accordion.Open(0)
	p.prefix.SetText("desk/")
	// Another writer (the CLI) changes the mode after the panel loaded.
	if err := worksubmission.SavePreferences(path, worksubmission.Preferences{Mode: worksubmission.PullRequestMode, BranchPrefix: "amux/"}); err != nil {
		t.Fatal(err)
	}
	p.save.OnTapped()
	want := worksubmission.Preferences{Mode: worksubmission.PullRequestMode, BranchPrefix: "desk/"}
	if got, err := worksubmission.LoadPreferences(path); err != nil || got != want {
		t.Fatalf("stale draft overwrote the other writer's mode: %+v, %v", got, err)
	}
	if p.draft != want || p.confirmed != want || p.mode.Selected != "Pull request" {
		t.Fatalf("panel did not adopt the saved value: draft=%+v confirmed=%+v mode=%q", p.draft, p.confirmed, p.mode.Selected)
	}
}
