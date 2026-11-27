//go:build desktop

package desktopapp

import (
	"context"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"github.com/nitecon/amux/desktop"
)

func showGatewayOnboarding(ctx context.Context, preferences, state string) error {
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
	stopShutdown := context.AfterFunc(ctx, func() { fyne.Do(application.Quit) })
	defer stopShutdown()
	window.ShowAndRun()
	if completed {
		return errRestartDesktop
	}
	return nil
}
