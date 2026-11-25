package desktop

import (
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/worksubmission"
)

type workSubmissionSettingsPanel struct {
	browser   *Browser
	path      string
	accordion *widget.Accordion
	content   *fyne.Container
	root      *container.Split
	mode      *widget.RadioGroup
	prefix    *widget.Entry
	status    *widget.Label
	save      *widget.Button
	cancel    *widget.Button

	confirmed worksubmission.Preferences
	draft     worksubmission.Preferences
	loadErr   error
	closed    bool
}

func newWorkSubmissionSettingsPanel(path string, browser *Browser) *workSubmissionSettingsPanel {
	p := &workSubmissionSettingsPanel{browser: browser, path: path, confirmed: worksubmission.DefaultPreferences()}
	loaded, err := worksubmission.LoadPreferences(path)
	if err == nil {
		p.confirmed = loaded
	}
	p.draft = p.confirmed
	p.loadErr = err
	p.mode = widget.NewRadioGroup([]string{"Commit and push", "Pull request"}, p.selectMode)
	p.mode.Horizontal = true
	p.prefix = widget.NewEntry()
	p.prefix.SetPlaceHolder("amux/")
	p.prefix.OnChanged = func(value string) {
		if p.isClosed() {
			return
		}
		p.draft.BranchPrefix = value
		p.updateButtons()
	}
	p.status = widget.NewLabel("")
	p.status.Wrapping = fyne.TextWrapWord
	intro := widget.NewLabel("Choose how completed task work is submitted. Saving does not start a worker or publish work.")
	intro.Wrapping = fyne.TextWrapWord
	p.save = widget.NewButton("Save", p.saveDraft)
	p.cancel = widget.NewButton("Cancel", p.cancelDraft)
	p.syncWidgets()
	if err != nil {
		p.setStatus("Could not load work submission preferences: " + err.Error())
	} else {
		p.setStatus("Work submission preferences loaded.")
	}
	p.content = container.NewVBox(
		intro,
		widget.NewForm(widget.NewFormItem("Submission mode", p.mode), widget.NewFormItem("Pull request branch prefix", p.prefix)),
		container.NewHBox(p.save, p.cancel),
		p.status,
	)
	p.accordion = widget.NewAccordion(widget.NewAccordionItem("Work submission settings", p.content))
	p.updateButtons()
	return p
}

func (p *workSubmissionSettingsPanel) selectMode(value string) {
	if p.isClosed() {
		return
	}
	switch value {
	case "Commit and push":
		p.draft.Mode = worksubmission.CommitAndPush
	case "Pull request":
		p.draft.Mode = worksubmission.PullRequestMode
	default:
		return
	}
	p.updateButtons()
}

func (p *workSubmissionSettingsPanel) saveDraft() {
	if p.isClosed() {
		return
	}
	// Send only the fields edited in this panel so a stale draft cannot
	// overwrite a field another writer changed since the panel loaded.
	var change worksubmission.PreferencesChange
	if p.draft.Mode != p.confirmed.Mode {
		change.Mode = &p.draft.Mode
	}
	if p.draft.BranchPrefix != p.confirmed.BranchPrefix {
		change.BranchPrefix = &p.draft.BranchPrefix
	}
	saved, err := worksubmission.UpdatePreferences(p.path, change)
	if err != nil {
		p.setStatus("Could not save work submission preferences: " + err.Error())
		p.updateButtons()
		return
	}
	p.confirmed = saved
	p.draft = saved
	p.syncWidgets()
	p.loadErr = nil
	p.setStatus("Work submission preferences saved.")
	p.updateButtons()
}

func (p *workSubmissionSettingsPanel) cancelDraft() {
	if p.isClosed() {
		return
	}
	p.draft = p.confirmed
	p.syncWidgets()
	if p.loadErr != nil {
		p.setStatus("Could not load work submission preferences: " + p.loadErr.Error())
	} else {
		p.setStatus("Draft changes discarded.")
	}
	p.updateButtons()
}

func (p *workSubmissionSettingsPanel) syncWidgets() {
	p.mode.SetSelected(workSubmissionModeLabel(p.draft.Mode))
	p.prefix.SetText(p.draft.BranchPrefix)
}

func (p *workSubmissionSettingsPanel) updateButtons() {
	p.save.Enable()
	p.cancel.Enable()
	if p.draft == p.confirmed {
		p.save.Disable()
		p.cancel.Disable()
	}
}

func workSubmissionModeLabel(mode worksubmission.Mode) string {
	switch mode {
	case worksubmission.PullRequestMode:
		return "Pull request"
	default:
		return "Commit and push"
	}
}

func (p *workSubmissionSettingsPanel) close() {
	p.save.Disable()
	p.cancel.Disable()
	p.mode.Disable()
	p.prefix.Disable()
	if p.loadErr != nil {
		p.setStatus(fmt.Sprintf("Could not load work submission preferences: %v", p.loadErr))
	}
	p.closed = true
}

func (p *workSubmissionSettingsPanel) isClosed() bool {
	if p.closed || p.browser == nil {
		return p.closed
	}
	p.browser.mu.Lock()
	defer p.browser.mu.Unlock()
	return p.browser.closing
}

func (p *workSubmissionSettingsPanel) setRoot(root *container.Split) {
	p.root = root
}

func (p *workSubmissionSettingsPanel) setStatus(message string) {
	if p.status.Text == message {
		return
	}
	p.status.SetText(message)
	if p.content != nil {
		p.content.Refresh()
	}
	if p.accordion != nil {
		p.accordion.Refresh()
	}
	if p.root != nil {
		p.root.Refresh()
	}
}
