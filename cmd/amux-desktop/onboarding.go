//go:build desktop

package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"github.com/nitecon/amux/desktop"
)

func showGatewayOnboarding(preferences, state string) error {
	application := app.New()
	application.Settings().SetTheme(desktop.WorkspaceTheme())
	window := application.NewWindow("Set up Amux")
	completed := false
	panel := desktop.GatewayOnboarding(desktop.GatewaySetupConfig{
		Preferences:     desktop.DefaultGatewayPreferences(),
		PreferencesPath: preferences,
		StateDirectory:  state,
		Restart: func() {
			completed = true
			application.Quit()
		},
	})
	window.SetContent(container.NewVScroll(panel))
	window.Resize(fyne.NewSize(720, 520))
	window.CenterOnScreen()
	window.ShowAndRun()
	if completed {
		return errRestartDesktop
	}
	return nil
}
