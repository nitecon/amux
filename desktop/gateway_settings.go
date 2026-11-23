package desktop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

type GatewaySetupConfig struct {
	Preferences        GatewayPreferences
	ActiveMode         GatewayMode
	PreferencesPath    string
	StateDirectory     string
	ServerEndpoint     string
	ConnectionOverride bool
	GeneratePeeringKey func(context.Context, bool) (string, error)
	Restart            func()
}

type gatewaySettingsPanel struct {
	accordion           *widget.Accordion
	config              GatewaySetupConfig
	mode                *widget.RadioGroup
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

func newGatewaySettingsPanel(config GatewaySetupConfig) *gatewaySettingsPanel {
	p := &gatewaySettingsPanel{config: config}
	p.mode = widget.NewRadioGroup([]string{"Server", "Client"}, func(string) { p.updateModeSections() })
	p.mode.Horizontal = true
	mode := config.Preferences.Mode
	if config.ConnectionOverride && (config.ActiveMode == GatewayModeServer || config.ActiveMode == GatewayModeClient) {
		mode = config.ActiveMode
	}
	if mode == GatewayModeClient {
		p.mode.SetSelected("Client")
	} else {
		p.mode.SetSelected("Server")
	}
	if config.ConnectionOverride {
		p.mode.Disable()
	}
	p.endpoint = widget.NewEntry()
	p.endpoint.SetPlaceHolder("https://gateway.example:7913")
	p.endpoint.SetText(config.Preferences.Endpoint)
	p.invitation = widget.NewMultiLineEntry()
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
	p.accordion = widget.NewAccordion(widget.NewAccordionItem("Gateway", p.content()))
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
	clientEnroll := widget.NewButton("Enroll with peering key", p.enroll)
	p.saveButton = widget.NewButton("Save mode and restart", p.saveMode)
	p.remoteDetails = container.NewVBox(widget.NewLabel("Advertised hostname or IP"), p.advertisedHost, container.NewHBox(generate, copyKey, layout.NewSpacer()), widget.NewLabel("Peering key"), p.key)
	p.serverSection = container.NewVBox(widget.NewLabel("Server mode"), p.endpointLabel, p.remoteHosting, p.remoteDetails)
	p.clientForm = container.NewVBox(widget.NewLabel("Gateway endpoint"), p.endpoint, widget.NewLabel("Peering key"), p.invitation, container.NewHBox(clientEnroll, layout.NewSpacer()))
	p.clientEndpointLabel = widget.NewLabel("Active gateway endpoint: " + p.config.ServerEndpoint)
	p.clientEndpointLabel.Wrapping = fyne.TextWrapWord
	p.clientSection = container.NewVBox(widget.NewLabel("Client mode · enroll with a gateway"), p.clientEndpointLabel, p.clientForm)
	modeControls := container.NewVBox(widget.NewLabel("Gateway mode"), p.mode, container.NewHBox(p.saveButton, layout.NewSpacer()))
	if p.config.ConnectionOverride {
		p.saveButton.Hide()
		p.remoteHosting.Hide()
		p.remoteDetails.Hide()
		p.clientForm.Hide()
	}
	p.updateServerEndpoint()
	p.updateModeSections()
	return container.NewVBox(modeControls, widget.NewSeparator(), p.serverSection, p.clientSection, p.status)
}

func (p *gatewaySettingsPanel) updateServerEndpoint() {
	if p.endpointLabel == nil {
		return
	}
	if p.config.ServerEndpoint != "" {
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

func (p *gatewaySettingsPanel) updateModeSections() {
	if p.serverSection == nil || p.clientSection == nil {
		return
	}
	if p.mode.Selected == "Client" {
		p.serverSection.Hide()
		p.clientSection.Show()
	} else {
		p.clientSection.Hide()
		p.serverSection.Show()
	}
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
	if p.mode.Selected == "Client" {
		if prefs.Endpoint == "" || prefs.EnrolledProfile == "" {
			p.status.SetText("Enroll with a peering key before switching to client mode.")
			return
		}
		prefs.Mode = GatewayModeClient
		prefs.RemoteHosting = false
	} else {
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
	}
	if err := SaveGatewayPreferences(p.config.PreferencesPath, prefs); err != nil {
		p.status.SetText(err.Error())
		return
	}
	p.config.Preferences = prefs
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
		fyne.Do(func() {
			p.busy = false
			if err != nil {
				p.status.SetText(err.Error())
				return
			}
			p.config.Preferences = prefs
			p.invitation.SetText("")
			p.key.SetText("")
			p.status.SetText("Enrolled securely. Restarting with the new connection…")
			if p.config.Restart != nil {
				p.config.Restart()
			}
		})
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
