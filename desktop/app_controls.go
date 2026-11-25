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
	content        *fyne.Container
	status         *widget.Label
	login          *widget.Check
	update         *widget.Button
	restart        *widget.Button
	cliPath        string
	stagedArchive  string
	restartDesktop func(string)
	stageUpdate    updateStage
}

func linuxLaunchItemContent(executable string) string {
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(executable)
	return fmt.Sprintf("[Desktop Entry]\nType=Application\nName=Amux\nExec=\"%s\"\nTerminal=false\nX-GNOME-Autostart-enabled=true\n", quoted)
}

func macLaunchItemContent(executable string) string {
	return fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict><key>Label</key><string>com.nitecon.amux</string><key>ProgramArguments</key><array><string>%s</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/></dict></plist>\n", html.EscapeString(executable))
}

// NewAppControlsPanel returns the desktop's verified update and startup controls.
func NewAppControlsPanel(cliPath string, restartDesktop func(string)) fyne.CanvasObject {
	return newAppControlsPanel(cliPath, restartDesktop, nil).content
}

func newAppControlsPanel(cliPath string, restartDesktop func(string), stage updateStage) *appControlsPanel {
	if stage == nil {
		stage = appupdate.Stage
	}
	p := &appControlsPanel{
		status: widget.NewLabel(""), cliPath: cliPath, restartDesktop: restartDesktop, stageUpdate: stage,
	}
	p.status.Wrapping = fyne.TextWrapWord
	p.update = widget.NewButton("Check and download update", p.checkForUpdates)
	p.restart = widget.NewButton("Install and restart", p.installAndRestart)
	p.restart.Hide()
	p.login = widget.NewCheck("Launch amux when I sign in", p.setLaunchAtLogin)
	p.login.Checked = launchAtLoginEnabled()
	p.content = container.NewVBox(
		widget.NewLabel("Updates and startup"),
		container.NewHBox(p.update, p.restart, p.login),
		p.status,
	)
	return p
}

func (p *appControlsPanel) checkForUpdates() {
	p.stagedArchive = ""
	p.restart.Hide()
	p.update.Disable()
	p.status.SetText("Checking for an amux update…")
	go func() {
		archive, update, err := p.stageUpdate(context.Background(), p.cliPath)
		fyne.Do(func() {
			p.update.Enable()
			if err != nil {
				p.status.SetText("Could not download the update: " + err.Error())
				return
			}
			if !update.Available {
				p.status.SetText("amux " + update.Version + " is up to date.")
				return
			}
			if update.ManualInstall || p.restartDesktop == nil {
				p.status.SetText("Downloaded and verified amux " + update.Version + ". Extract it to a new location and launch amux from there.\n" + archive)
				return
			}
			p.stagedArchive = archive
			p.restart.Show()
			p.status.SetText("Verified amux " + update.Version + " is ready. Install and restart when you are ready.")
		})
	}()
}

func (p *appControlsPanel) installAndRestart() {
	if p.stagedArchive == "" || p.restartDesktop == nil {
		p.status.SetText("No verified update is ready to install.")
		return
	}
	p.status.SetText("Closing amux after the verified update helper is scheduled…")
	p.restartDesktop(p.stagedArchive)
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
