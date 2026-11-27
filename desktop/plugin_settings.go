package desktop

import (
	"context"
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

var pluginSettingLabels = map[string]string{
	"assignment": "Automatic assignment",
	"notifier":   "Task notifications",
	"reminder":   "Task reminders",
	"review":     "Automatic reviews",
	"reclaim":    "Recover work",
}

var providerSettingLabels = map[string]string{
	"antigravity": "Antigravity",
	"claude":      "Claude",
	"codex":       "Codex",
}

// switchSettingsSource describes one server-side set of on/off switches.
// save may return a status note shown after a confirmed write.
type switchSettingsSource struct {
	noun   string
	title  string
	note   string
	ids    []string
	labels map[string]string
	load   func(context.Context) ([]storage.PluginSetting, error)
	save   func(context.Context, string, bool) (*storage.PluginSetting, string, error)
}

type pluginSettingsPanel struct {
	browser   *Browser
	source    switchSettingsSource
	checks    map[string]*widget.Check
	refresh   *widget.Button
	status    *widget.Label
	accordion *widget.Accordion
	root      *container.Split

	confirmed       map[string]bool
	loaded          bool
	loading         bool
	writing         bool
	uncertain       bool
	closed          bool
	programmatic    bool
	readGeneration  uint64
	writeGeneration uint64
}

func newPluginSettingsPanel(browser *Browser) *pluginSettingsPanel {
	return newSwitchSettingsPanel(browser, switchSettingsSource{
		noun:   "plugin",
		title:  "Server plugin settings",
		note:   "Changes apply across this server. Automatic reviews apply to future completions; queued reviews remain available.",
		ids:    storage.SwitchablePluginIDs(),
		labels: pluginSettingLabels,
		load: func(ctx context.Context) ([]storage.PluginSetting, error) {
			return browser.client.PluginSettings(ctx)
		},
		save: func(ctx context.Context, id string, enabled bool) (*storage.PluginSetting, string, error) {
			setting, err := browser.client.SetPluginEnabled(ctx, id, enabled)
			return setting, "", err
		},
	})
}

func newProviderSettingsPanel(browser *Browser) *pluginSettingsPanel {
	return newSwitchSettingsPanel(browser, switchSettingsSource{
		noun:   "provider",
		title:  "Agent providers",
		note:   "Disabled providers receive no new tasks. Disabling a provider returns its active tasks to the queue for enabled agents.",
		ids:    storage.SwitchableProviderIDs(),
		labels: providerSettingLabels,
		load: func(ctx context.Context) ([]storage.PluginSetting, error) {
			settings, err := browser.client.ProviderSettings(ctx)
			converted := make([]storage.PluginSetting, len(settings))
			for i, setting := range settings {
				converted[i] = storage.PluginSetting{ID: setting.ID, Enabled: setting.Enabled}
			}
			return converted, err
		},
		save: func(ctx context.Context, id string, enabled bool) (*storage.PluginSetting, string, error) {
			setting, released, err := browser.client.SetProviderEnabled(ctx, id, enabled)
			if err != nil || setting == nil {
				return nil, "", err
			}
			note := ""
			if !enabled {
				note = fmt.Sprintf(" %d active task(s) returned to the queue.", released)
			}
			return &storage.PluginSetting{ID: setting.ID, Enabled: setting.Enabled}, note, nil
		},
	})
}

func newSwitchSettingsPanel(browser *Browser, source switchSettingsSource) *pluginSettingsPanel {
	p := &pluginSettingsPanel{
		browser:   browser,
		source:    source,
		checks:    make(map[string]*widget.Check),
		confirmed: make(map[string]bool),
		status:    widget.NewLabel("Loading server settings…"),
	}
	p.status.Wrapping = fyne.TextWrapWord
	rows := make([]fyne.CanvasObject, 0, len(source.ids)+3)
	for _, id := range source.ids {
		check := widget.NewCheck(source.labels[id], nil)
		check.Disable()
		check.OnChanged = func(enabled bool) { p.setEnabled(id, enabled) }
		p.checks[id] = check
		rows = append(rows, check)
	}
	note := widget.NewLabel(source.note)
	note.Wrapping = fyne.TextWrapWord
	rows = append(rows, note)
	p.refresh = widget.NewButton("Reload settings", func() { p.reload(true) })
	p.refresh.Disable()
	rows = append(rows, p.refresh, p.status)
	p.accordion = widget.NewAccordion(widget.NewAccordionItem(source.title, container.NewVBox(rows...)))
	return p
}

// reload uses an explicit read to recover a write whose acknowledgement was
// uncertain. Startup and user refreshes also read the server's current values.
func (p *pluginSettingsPanel) reload(explicit bool) {
	if p.isClosed() || p.closed || p.loading || p.writing || (p.uncertain && !explicit) {
		return
	}
	p.readGeneration++
	readGeneration := p.readGeneration
	writeGeneration := p.writeGeneration
	p.loading = true
	p.setStatus("Loading server " + p.source.noun + " settings…")
	p.updateControls()
	if !p.browser.launch(func(ctx context.Context) {
		settings, err := p.source.load(ctx)
		p.browser.dispatch(func() { p.finishReload(readGeneration, writeGeneration, settings, err) })
	}) {
		p.loading = false
		p.setStatus("Server settings are unavailable until the browser is started.")
		p.updateControls()
	}
}

func (p *pluginSettingsPanel) finishReload(readGeneration, writeGeneration uint64, settings []storage.PluginSetting, err error) {
	if p.isClosed() || p.closed || readGeneration != p.readGeneration || writeGeneration != p.writeGeneration || p.writing {
		return
	}
	p.loading = false
	if err != nil {
		p.uncertain = true
		p.setStatus("Could not load server " + p.source.noun + " settings: " + err.Error())
		p.updateControls()
		return
	}
	ids := p.source.ids
	if len(settings) != len(ids) {
		p.uncertain = true
		p.setStatus("Could not load server " + p.source.noun + " settings: incomplete server response.")
		p.updateControls()
		return
	}
	for i, id := range ids {
		if settings[i].ID != id {
			p.uncertain = true
			p.setStatus("Could not load server " + p.source.noun + " settings: invalid server response order.")
			p.updateControls()
			return
		}
	}
	p.confirmed = make(map[string]bool, len(settings))
	p.programmatic = true
	for _, setting := range settings {
		p.confirmed[setting.ID] = setting.Enabled
		p.checks[setting.ID].SetChecked(setting.Enabled)
	}
	p.programmatic = false
	p.loaded = true
	p.uncertain = false
	p.setStatus("Server " + p.source.noun + " settings loaded.")
	p.updateControls()
}

func (p *pluginSettingsPanel) setEnabled(id string, enabled bool) {
	if p.programmatic {
		return
	}
	confirmed, known := p.confirmed[id]
	if p.isClosed() || p.closed {
		if check := p.checks[id]; known && check != nil {
			p.programmatic = true
			check.SetChecked(confirmed)
			p.programmatic = false
		}
		return
	}
	if !p.loaded || !known || p.loading || p.writing || p.uncertain || p.closed {
		p.programmatic = true
		p.checks[id].SetChecked(confirmed)
		p.programmatic = false
		p.updateControls()
		return
	}
	// A toggle is a request, not a confirmed setting. Restore the displayed
	// server value until the matching acknowledgement arrives.
	p.programmatic = true
	p.checks[id].SetChecked(confirmed)
	p.programmatic = false
	p.writing = true
	p.writeGeneration++
	writeGeneration := p.writeGeneration
	p.setStatus("Saving server " + p.source.noun + " setting…")
	p.updateControls()
	if !p.browser.launch(func(ctx context.Context) {
		setting, note, err := p.source.save(ctx, id, enabled)
		p.browser.dispatch(func() { p.finishWrite(writeGeneration, id, enabled, setting, note, err) })
	}) {
		p.writing = false
		p.uncertain = true
		p.setStatus("Could not confirm the server setting. Reload settings before changing it.")
		p.updateControls()
	}
}

func (p *pluginSettingsPanel) finishWrite(writeGeneration uint64, id string, enabled bool, setting *storage.PluginSetting, note string, err error) {
	if p.isClosed() || p.closed || writeGeneration != p.writeGeneration || !p.writing {
		return
	}
	p.writing = false
	if err != nil {
		p.uncertain = true
		p.setStatus(fmt.Sprintf("Could not confirm %s. Reload settings before changing it: %s", p.source.labels[id], err))
		p.updateControls()
		return
	}
	if setting == nil || setting.ID != id || setting.Enabled != enabled {
		p.uncertain = true
		p.setStatus("The server setting acknowledgement was invalid. Reload settings before changing it.")
		p.updateControls()
		return
	}
	p.confirmed[id] = enabled
	p.programmatic = true
	p.checks[id].SetChecked(enabled)
	p.programmatic = false
	p.setStatus("Server " + p.source.noun + " setting saved." + note)
	p.updateControls()
}

func (p *pluginSettingsPanel) updateControls() {
	closed := p.isClosed()
	locked := closed || !p.loaded || p.loading || p.writing || p.uncertain
	for _, check := range p.checks {
		if locked {
			check.Disable()
		} else {
			check.Enable()
		}
	}
	if closed || p.loading || p.writing {
		p.refresh.Disable()
	} else {
		p.refresh.Enable()
	}
}

func (p *pluginSettingsPanel) setRoot(root *container.Split) {
	p.root = root
}

func (p *pluginSettingsPanel) setStatus(message string) {
	if p.status.Text == message {
		return
	}
	p.status.SetText(message)
	if p.root != nil {
		p.root.Refresh()
	}
}

func (p *pluginSettingsPanel) isClosed() bool {
	p.browser.mu.Lock()
	defer p.browser.mu.Unlock()
	return p.browser.closing
}

func (p *pluginSettingsPanel) close() {
	p.closed = true
	p.updateControls()
}
