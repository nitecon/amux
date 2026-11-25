package desktop

import (
	"context"
	"path/filepath"
	"testing"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
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

func TestClientOnGatewayHostOffersPeeringKey(t *testing.T) {
	application := fynetest.NewApp()
	defer application.Quit()
	requests := make(chan bool, 1)
	config := GatewaySetupConfig{
		Preferences: GatewayPreferences{Mode: GatewayModeClient, Endpoint: "https://host:7913", EnrolledProfile: "client-profile.json"},
		ActiveMode:  GatewayModeClient, ServerEndpoint: "https://host:7913",
		GeneratePeeringKey: func(_ context.Context, copyCurrent bool) (string, error) {
			requests <- copyCurrent
			return "peering-key", nil
		},
	}
	if newGatewaySettingsPanel(config).hostActions != nil {
		t.Fatal("a client of a remote gateway must not offer host peering actions")
	}
	config.HostedOnThisMachine = true
	panel := newGatewaySettingsPanel(config)
	if panel.hostActions == nil || !panel.clientSection.Visible() {
		t.Fatal("a client on the gateway host should offer peering key actions")
	}
	fynetest.Tap(panel.hostActions.Objects[2].(*fyne.Container).Objects[0].(*widget.Button))
	if copyCurrent := <-requests; copyCurrent {
		t.Fatal("generate requested the current key instead of a new one")
	}
}
