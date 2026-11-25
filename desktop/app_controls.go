package desktop

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/appupdate"
)

type updateStage func(context.Context, string) (string, appupdate.UpdateInfo, error)

type appControlsPanel struct {
	content       *fyne.Container
	status        *widget.Label
	login         *widget.Check
	update        *widget.Button
	cliPath       string
	installUpdate func(string) error
	stageUpdate   updateStage
}

func linuxLaunchItemContent(executable string) string {
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(executable)
	return fmt.Sprintf("[Desktop Entry]\nType=Application\nName=Amux\nExec=\"%s\"\nTerminal=false\nX-GNOME-Autostart-enabled=true\n", quoted)
}

func macLaunchItemContent(executable string) string {
	return fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict><key>Label</key><string>com.nitecon.amux</string><key>ProgramArguments</key><array><string>%s</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/></dict></plist>\n", html.EscapeString(executable))
}

// NewAppControlsPanel returns the desktop's verified update and startup controls.
func NewAppControlsPanel(cliPath string, installUpdate func(string) error) fyne.CanvasObject {
	return newAppControlsPanel(cliPath, installUpdate, nil).content
}

func newAppControlsPanel(cliPath string, installUpdate func(string) error, stage updateStage) *appControlsPanel {
	if stage == nil {
		stage = appupdate.Stage
	}
	p := &appControlsPanel{
		status: widget.NewLabel(""), cliPath: cliPath, installUpdate: installUpdate, stageUpdate: stage,
	}
	p.status.Wrapping = fyne.TextWrapWord
	p.update = widget.NewButton("Update now", p.checkForUpdates)
	p.login = widget.NewCheck("Launch amux when I sign in", p.setLaunchAtLogin)
	p.login.Checked = launchAtLoginEnabled()
	p.content = container.NewVBox(
		widget.NewLabel("Updates and startup"),
		container.NewHBox(p.update, p.login),
		p.status,
	)
	return p
}

func (p *appControlsPanel) checkForUpdates() {
	p.update.Disable()
	p.status.SetText("Checking for an amux update…")
	go func() {
		archive, update, err := p.stageUpdate(context.Background(), p.cliPath)
		if err != nil {
			p.finishUpdate("Could not download the update: " + err.Error())
			return
		}
		if !update.Available {
			p.finishUpdate("amux " + update.Version + " is up to date.")
			return
		}
		if p.installUpdate == nil {
			p.finishUpdate("The updater is unavailable.")
			return
		}
		fyne.Do(func() { p.status.SetText("Installing amux " + update.Version + " and restarting…") })
		if err := p.installUpdate(archive); err != nil {
			p.finishUpdate("Could not install the update: " + err.Error())
		}
	}()
}

func (p *appControlsPanel) finishUpdate(status string) {
	fyne.Do(func() {
		p.status.SetText(status)
		p.update.Enable()
	})
}

func (p *appControlsPanel) setLaunchAtLogin(enabled bool) {
	if err := setLaunchAtLogin(enabled); err != nil {
		onChanged := p.login.OnChanged
		p.login.OnChanged = nil
		p.login.SetChecked(!enabled)
		p.login.OnChanged = onChanged
		p.status.SetText("Could not change launch at sign in: " + err.Error())
		return
	}
	if enabled {
		p.status.SetText("amux will launch when you sign in.")
	} else {
		p.status.SetText("amux will not launch when you sign in.")
	}
}

func launchItemFile(path, executable, content string) error {
	if strings.TrimSpace(executable) == "" || !filepath.IsAbs(executable) {
		return fmt.Errorf("amux executable path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".amux-launch-*")
	if err != nil {
		return err
	}
	tempPath := tmp.Name()
	defer os.Remove(tempPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
