package desktop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

type GatewaySetupConfig struct {
	FirstLaunch        bool
	Preferences        GatewayPreferences
	ActiveMode         GatewayMode
	PreferencesPath    string
	StateDirectory     string
	ServerEndpoint     string
	ConnectionOverride bool
	GeneratePeeringKey func(context.Context, bool) (string, error)
	Apply              func(context.Context, GatewayPreferences) (GatewayPreferences, error)
	Restart            func()
}

type gatewaySettingsPanel struct {
	body                fyne.CanvasObject
	config              GatewaySetupConfig
	mode                GatewayMode
	serverMode          *widget.Button
	clientMode          *widget.Button
	endpoint            *widget.Entry
	invitation          *widget.Entry
	key                 *widget.Entry
	status              *widget.Label
	serverSection       *fyne.Container
	clientSection       *fyne.Container
	endpointLabel       *widget.Label
	clientEndpointLabel *widget.Label
	remoteHosting       *widget.Check
	advertisedHost      *widget.Entry
	remoteDetails       *fyne.Container
	keyActions          []*widget.Button
	saveButton          *widget.Button
	clientForm          *fyne.Container
	busy                bool
}

// GatewayOnboarding renders the gateway choices before a connection exists.
func GatewayOnboarding(config GatewaySetupConfig) fyne.CanvasObject {
	config.FirstLaunch = true
	return newGatewaySettingsPanel(config).body
}

func newGatewaySettingsPanel(config GatewaySetupConfig) *gatewaySettingsPanel {
	p := &gatewaySettingsPanel{config: config}
	p.mode = GatewayModeServer
	if config.Preferences.Mode == GatewayModeClient {
		p.mode = GatewayModeClient
	}
	if config.FirstLaunch {
		p.mode = ""
	}
	if config.ConnectionOverride && config.ActiveMode != "" {
		p.mode = config.ActiveMode
	}
	p.endpoint = widget.NewEntry()
	p.endpoint.SetPlaceHolder("https://gateway.example:24182")
	p.endpoint.SetText(config.Preferences.Endpoint)
	p.invitation = widget.NewPasswordEntry()
	p.invitation.SetPlaceHolder("Paste the peering key provided by the server")
	p.key = widget.NewMultiLineEntry()
	p.key.Disable()
	p.remoteHosting = widget.NewCheck("Enable remote hosting with secure mTLS", func(bool) { p.updateServerEndpoint() })
	p.remoteHosting.SetChecked(config.Preferences.RemoteHosting)
	p.advertisedHost = widget.NewEntry()
	p.advertisedHost.SetPlaceHolder("Advertised hostname or IP address")
	p.advertisedHost.SetText(config.Preferences.AdvertisedHost)
	p.advertisedHost.OnChanged = func(string) { p.updateServerEndpoint() }
	p.status = widget.NewLabel("")
	p.status.Wrapping = fyne.TextWrapWord
	p.body = p.content()
	if config.ConnectionOverride {
		p.status.SetText("Command-line connection settings override the saved mode for this launch.")
	}
	return p
}

func (p *gatewaySettingsPanel) content() fyne.CanvasObject {
	p.endpointLabel = widget.NewLabel("")
	p.endpointLabel.Wrapping = fyne.TextWrapWord
	generate := widget.NewButton("Generate peering key", func() { p.copyPeeringKey(false) })
	copyKey := widget.NewButton("Copy current key", func() { p.copyPeeringKey(true) })
	p.keyActions = []*widget.Button{generate, copyKey}
	clientEnroll := widget.NewButtonWithIcon("Connect", theme.LoginIcon(), p.enroll)
	clientEnroll.Importance = widget.HighImportance
	p.invitation.OnSubmitted = func(string) { p.enroll() }
	p.saveButton = widget.NewButton("Save mode and restart", p.saveMode)
	p.remoteDetails = container.NewVBox(widget.NewLabel("Advertised hostname or IP"), p.advertisedHost, container.NewHBox(generate, copyKey, layout.NewSpacer()), widget.NewLabel("Peering key"), p.key)
	p.serverSection = settingsCard(container.NewVBox(p.endpointLabel, p.remoteHosting, p.remoteDetails, container.NewHBox(p.saveButton, layout.NewSpacer())))
	p.clientForm = container.NewVBox(widget.NewLabel("Server address"), p.endpoint, widget.NewLabel("Peering key"), p.invitation, container.NewHBox(clientEnroll, layout.NewSpacer()))
	p.clientEndpointLabel = widget.NewLabel("")
	p.clientEndpointLabel.Wrapping = fyne.TextWrapWord
	if p.config.ActiveMode == GatewayModeClient && p.config.ServerEndpoint != "" {
		p.clientEndpointLabel.SetText("Current gateway · " + p.config.ServerEndpoint)
	} else {
		p.clientEndpointLabel.Hide()
	}
	p.clientSection = settingsCard(container.New(layout.NewCustomPaddedVBoxLayout(10),
		widget.NewLabel("Join an existing gateway with its address and a temporary peering key."), p.clientForm, p.clientEndpointLabel))
	p.serverMode = widget.NewButtonWithIcon("Server", theme.StorageIcon(), func() { p.selectMode(GatewayModeServer) })
	p.clientMode = widget.NewButtonWithIcon("Client", theme.ComputerIcon(), func() { p.selectMode(GatewayModeClient) })
	modeControls := container.NewGridWithColumns(2,
		settingsCard(container.NewVBox(p.serverMode, widget.NewLabel("Host projects and tasks on this device."))),
		settingsCard(container.NewVBox(p.clientMode, widget.NewLabel("Join a gateway on another machine."))),
	)

	if p.config.ConnectionOverride {
		p.serverMode.Disable()
		p.clientMode.Disable()
		p.saveButton.Hide()
		p.remoteHosting.Hide()
		p.remoteDetails.Hide()
		p.clientForm.Hide()
	}
	heading := browserSectionHeading("OPERATING MODE")
	if p.config.FirstLaunch {
		heading.Text = "SET UP AMUX"
		p.serverMode.SetText("Host a gateway")
		p.clientMode.SetText("Join an existing gateway")
		p.saveButton.SetText("Start gateway and connect")
		guidance := widget.NewLabel("For one machine, leave remote hosting off. Amux starts a local gateway and connects this client automatically. To enroll other machines later, generate a peering key in Settings.")
		guidance.Wrapping = fyne.TextWrapWord
		p.serverSection.Add(guidance)
		p.remoteDetails.Objects = []fyne.CanvasObject{widget.NewLabel("Advertised hostname or IP"), p.advertisedHost}
	}
	p.updateServerEndpoint()
	p.updateModeSections()
	return container.New(layout.NewCustomPaddedVBoxLayout(14), heading, container.NewThemeOverride(modeControls, toolbarTheme{Theme: fyne.CurrentApp().Settings().Theme()}), p.serverSection, p.clientSection, p.status)
}

func (p *gatewaySettingsPanel) updateServerEndpoint() {
	if p.endpointLabel == nil {
		return
	}
	if p.config.FirstLaunch {
		p.endpointLabel.SetText("This client will connect to the local gateway at https://127.0.0.1:7913.")
	} else if p.config.ServerEndpoint != "" {
		p.endpointLabel.SetText("Active gateway endpoint: " + p.config.ServerEndpoint)
	} else {
		p.endpointLabel.SetText("Active gateway endpoint: http://127.0.0.1:7913 (local only)")
	}
	for _, button := range p.keyActions {
		if p.remoteHosting.Checked && p.config.GeneratePeeringKey != nil && !p.config.ConnectionOverride {
			button.Enable()
		} else {
			button.Disable()
		}
	}
	if p.remoteHosting.Checked && !p.config.ConnectionOverride {
		p.remoteDetails.Show()
	} else {
		p.remoteDetails.Hide()
	}
}

func (p *gatewaySettingsPanel) selectMode(mode GatewayMode) {
	if p.busy || p.config.ConnectionOverride {
		return
	}
	p.mode = mode
	p.status.SetText("")
	p.updateModeSections()
}

func (p *gatewaySettingsPanel) updateModeSections() {
	p.serverMode.Importance = widget.MediumImportance
	p.clientMode.Importance = widget.MediumImportance
	if p.mode == "" {
		p.serverSection.Hide()
		p.clientSection.Hide()
	} else if p.mode == GatewayModeClient {
		p.clientMode.Importance = widget.HighImportance
		p.serverSection.Hide()
		p.clientSection.Show()
	} else {
		p.serverMode.Importance = widget.HighImportance
		p.clientSection.Hide()
		p.serverSection.Show()
	}
	p.serverMode.Refresh()
	p.clientMode.Refresh()
}

func (p *gatewaySettingsPanel) saveMode() {
	if p.busy {
		return
	}
	if p.config.ConnectionOverride {
		p.status.SetText("Remove --server or --enrolled-profile to use saved Gateway mode.")
		return
	}
	prefs := p.config.Preferences
	prefs.Mode = GatewayModeServer
	prefs.Endpoint = ""
	prefs.EnrolledProfile = ""
	prefs.RemoteHosting = p.remoteHosting.Checked
	if prefs.RemoteHosting {
		host, err := NormalizeAdvertisedHost(p.advertisedHost.Text)
		if err != nil {
			p.status.SetText(err.Error())
			return
		}
		prefs.AdvertisedHost = host
	}

	if p.config.Apply == nil {
		p.finishGatewayChange(prefs, SaveGatewayPreferences(p.config.PreferencesPath, prefs))
		return
	}
	p.busy = true
	p.status.SetText("Updating local client configuration…")
	go func() {
		updated, err := p.config.Apply(context.Background(), prefs)
		if err == nil {
			err = SaveGatewayPreferences(p.config.PreferencesPath, updated)
		}
		fyne.Do(func() { p.finishGatewayChange(updated, err) })
	}()
}

func (p *gatewaySettingsPanel) finishGatewayChange(prefs GatewayPreferences, err error) {
	p.busy = false
	if err != nil {
		p.status.SetText(err.Error())
		return
	}
	p.config.Preferences = prefs
	p.invitation.SetText("")
	p.key.SetText("")
	p.status.SetText("Saved. Restarting gateway connection…")
	if p.config.Restart != nil {
		p.config.Restart()
	}
}

func (p *gatewaySettingsPanel) copyPeeringKey(copyCurrent bool) {
	if p.busy || p.config.GeneratePeeringKey == nil {
		return
	}
	p.busy = true
	p.status.SetText("Requesting peering key…")
	go func() {
		key, err := p.config.GeneratePeeringKey(context.Background(), copyCurrent)
		fyne.Do(func() {
			p.busy = false
			if err != nil {
				p.status.SetText(err.Error())
				return
			}
			p.key.SetText(key)
			fyne.CurrentApp().Clipboard().SetContent(key)
			p.status.SetText("Peering key copied to clipboard. Share it with the client once.")
		})
	}()
}

func (p *gatewaySettingsPanel) enroll() {
	if p.busy {
		return
	}
	if p.config.ConnectionOverride {
		p.status.SetText("Remove --server or --enrolled-profile before changing Gateway mode.")
		return
	}
	endpoint := strings.TrimSpace(p.endpoint.Text)
	invitation := strings.TrimSpace(p.invitation.Text)
	if endpoint == "" || invitation == "" {
		p.status.SetText("Enter the server endpoint and peering key.")
		return
	}
	p.busy = true
	p.status.SetText("Enrolling this device…")
	go func() {
		prefs, err := completeGatewayEnrollment(context.Background(), p.config.StateDirectory, p.config.PreferencesPath, p.config.Preferences, endpoint, invitation)
		if err == nil && p.config.Apply != nil {
			prefs, err = p.config.Apply(context.Background(), prefs)
		}
		fyne.Do(func() { p.finishGatewayChange(prefs, err) })
	}()
}

func completeGatewayEnrollment(ctx context.Context, stateDirectory, preferencesPath string, current GatewayPreferences, endpoint, invitation string) (GatewayPreferences, error) {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(invitation) == "" {
		return GatewayPreferences{}, errors.New("server endpoint and peering key are required")
	}
	peer, err := storage.PairRemote(ctx, stateDirectory, endpoint, invitation)
	if err != nil {
		return GatewayPreferences{}, fmt.Errorf("enroll with gateway: %w", err)
	}
	prefs := current
	prefs.Mode = GatewayModeClient
	prefs.Endpoint = peer.Profile.URL
	prefs.EnrolledProfile = peer.ProfilePath
	prefs.RemoteHosting = false
	if err := SaveGatewayPreferences(preferencesPath, prefs); err != nil {
		return GatewayPreferences{}, err
	}
	return prefs, nil
}
