package desktop

import (
	"path/filepath"
	"testing"

	fynetest "fyne.io/fyne/v2/test"
)

func TestFirstLaunchAsksBeforeSavingGatewayMode(t *testing.T) {
	application := fynetest.NewApp()
	defer application.Quit()
	path := filepath.Join(t.TempDir(), "gateway.json")
	restarts := 0
	panel := newGatewaySettingsPanel(GatewaySetupConfig{
		FirstLaunch: true, Preferences: DefaultGatewayPreferences(), PreferencesPath: path,
		Restart: func() { restarts++ },
	})
	if panel.serverSection.Visible() || panel.clientSection.Visible() {
		t.Fatal("onboarding should show the mode choices first")
	}
	fynetest.Tap(panel.clientMode)
	if !panel.clientSection.Visible() || panel.serverSection.Visible() || restarts != 0 {
		t.Fatal("joining should offer the enrollment fields")
	}
	panel.enroll()
	if restarts != 0 {
		t.Fatal("started without gateway address and peering key")
	}
	fynetest.Tap(panel.serverMode)
	fynetest.Tap(panel.saveButton)
	prefs, err := LoadGatewayPreferences(path)
	if err != nil || prefs.Mode != GatewayModeServer || prefs.RemoteHosting || restarts != 1 {
		t.Fatalf("single-machine setup stored %+v, restarted %d times, error %v", prefs, restarts, err)
	}
}
