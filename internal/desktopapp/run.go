//go:build desktop

package desktopapp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/desktop"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/update"
	"github.com/nitecon/amux/worksubmission"
	"github.com/rs/zerolog/log"
)

var errRestartDesktop = errors.New("restart desktop")

func Run(ctx context.Context, args []string) error {
	err := run(ctx, args)
	if errors.Is(err, errRestartDesktop) {
		return relaunchDesktop(args)
	}
	return err
}

// GLFW's desktop driver runs once per process. Relaunch only after run has
// released the previous gateway, worker journal, and other application state.
func relaunchDesktop(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate desktop for restart: %w", err)
	}
	if err := update.RelaunchDesktop(executable, args); err != nil {
		return fmt.Errorf("restart desktop: %w", err)
	}
	return nil
}

func run(parent context.Context, args []string) error {
	serverURL, enrolledProfile := "", ""
	keyEnv := "GATEWAY_API_KEY"
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("locate desktop configuration: %w", err)
	}
	gatewayStateDirectory, err := filepath.Abs(filepath.Join(configDirectory, "amux"))
	if err != nil {
		return fmt.Errorf("resolve gateway state directory: %w", err)
	}
	gatewayPreferencesPath := filepath.Join(gatewayStateDirectory, "gateway.json")
	preferences := filepath.Join(gatewayStateDirectory, "navigation.json")
	gatewayPrefs, gatewayPrefsErr := desktop.LoadGatewayPreferences(gatewayPreferencesPath)
	if gatewayPrefsErr != nil {
		gatewayPrefs = desktop.DefaultGatewayPreferences()
	}
	clientConfigPath, err := config.DefaultClientPath()
	if err != nil {
		return err
	}
	loadErr := config.LoadClient(clientConfigPath)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return fmt.Errorf("load local client configuration: %w", loadErr)
	}
	existingClientConfig := loadErr == nil
	var clientSettings config.Client
	var selectedHostedGateway *config.HostedGateway
	if existingClientConfig {
		clientSettings = config.Get()
		enrolledProfile = clientSettings.EnrolledProfile
		serverURL = clientSettings.GatewayEndpoint
		if clientSettings.APIKeyEnv != "" {
			keyEnv = clientSettings.APIKeyEnv
		}
		selectedHostedGateway = clientSettings.HostedGateway
		gatewayPrefs.Mode = desktop.GatewayModeClient
		gatewayPrefs.Endpoint, gatewayPrefs.EnrolledProfile = serverURL, enrolledProfile
		if selectedHostedGateway != nil {
			gatewayPrefs.Mode = desktop.GatewayModeServer
			gatewayPrefs.RemoteHosting = selectedHostedGateway.RemoteHosting
			gatewayPrefs.AdvertisedHost = selectedHostedGateway.AdvertisedHost
			gatewayPrefs.ServerInstanceID = selectedHostedGateway.InstanceID
		}
	}
	if !existingClientConfig && gatewayPrefsErr != nil {
		return gatewayPrefsErr
	}
	if !existingClientConfig {
		if _, err := os.Stat(gatewayPreferencesPath); errors.Is(err, os.ErrNotExist) {
			return showGatewayOnboarding(parent, gatewayPreferencesPath, gatewayStateDirectory)
		} else if err != nil {
			return err
		}
	}
	if !existingClientConfig && gatewayPrefs.Mode == desktop.GatewayModeClient {
		serverURL = gatewayPrefs.Endpoint
		enrolledProfile = gatewayPrefs.EnrolledProfile
	}
	workPreferencesPath, err := worksubmission.OptionalPreferencesPath(clientSettings.WorkPreferences, os.Getenv("AMUX_WORK_SUBMISSION_PREFERENCES"))
	if err != nil {
		return fmt.Errorf("resolve work submission preferences: %w", err)
	}
	key := os.Getenv(keyEnv)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	managedClient := false
	attachClient := func(endpoint string, hosted *config.HostedGateway) error {
		if !existingClientConfig {
			if err := writeClientConfig(ctx, clientConfigPath, enrolledProfile, endpoint, preferences, "", true, hosted); err != nil {
				return fmt.Errorf("configure local client: %w", err)
			}
			if err := config.LoadClient(clientConfigPath); err != nil {
				return err
			}
		}
		cli, err := amuxCLIPath()
		if err != nil {
			return err
		}
		if err := startOrAttachClient(ctx, cli, clientConfigPath, endpoint); err != nil {
			return err
		}
		managedClient = true
		return nil
	}
	serverProfile, serverEndpoint := "", ""
	if !existingClientConfig && gatewayPrefs.Mode == desktop.GatewayModeServer {
		serverProfile = gatewayStateDirectory
		hosted, err := hostedGatewaySettings(&gatewayPrefs)
		if err != nil {
			return err
		}
		if err := desktop.SaveGatewayPreferences(gatewayPreferencesPath, gatewayPrefs); err != nil {
			return err
		}
		advertisedHost := hosted.AdvertisedHost
		serverURL = config.LocalGatewayEndpoint
		enrolledProfile = filepath.Join(serverProfile, "local-client.json")
		serverEndpoint = "https://" + net.JoinHostPort(advertisedHost, "7913")
		if err := attachClient(serverURL, hosted); err != nil {
			return err
		}
	} else if selectedHostedGateway != nil {
		serverProfile = filepath.Dir(enrolledProfile)
		serverURL = config.LocalGatewayEndpoint
		serverEndpoint = "https://" + net.JoinHostPort(selectedHostedGateway.AdvertisedHost, "7913")
		if err := attachClient(serverURL, selectedHostedGateway); err != nil {
			return err
		}
	}
	if enrolledProfile == "" && key == "" {
		return fmt.Errorf("gateway API credential is not configured in %s", keyEnv)
	}
	var client *gatewayclient.Client
	if enrolledProfile != "" {
		enrolledProfile, err = filepath.Abs(enrolledProfile)
		if err == nil {
			client, err = gatewayclient.NewEnrolledFromManifest(serverURL, enrolledProfile)
		}
	} else {
		client, err = gatewayclient.New(serverURL, key, nil)
	}
	if err != nil {
		return err
	}
	if serverEndpoint == "" {
		serverEndpoint = client.Endpoint()
	}
	if !managedClient {
		if err := attachClient(client.Endpoint(), nil); err != nil {
			return err
		}
	}
	fleetCtx, stopFleet := context.WithTimeout(ctx, 3*time.Second)
	if err := config.LoadClientFleet(fleetCtx, clientConfigPath, client); err != nil {
		log.Warn().Err(err).Msg("load desktop fleet settings")
	}
	stopFleet()
	activeGatewayMode := gatewayPrefs.Mode

	chatRoot := filepath.Join(filepath.Dir(clientConfigPath), "conversations")

	windowApp := app.New()
	windowApp.Settings().SetTheme(desktop.WorkspaceTheme())
	restartRequested := false
	var view *desktop.Browser
	var generatePeeringKey func(context.Context, bool) (string, error)
	if serverProfile != "" && (gatewayPrefs.RemoteHosting || selectedHostedGateway != nil && selectedHostedGateway.RemoteHosting) {
		generatePeeringKey = func(keyCtx context.Context, copyCurrent bool) (string, error) {
			return requestServerPeeringKey(keyCtx, serverProfile, copyCurrent)
		}
	}
	hostedOnThisMachine := false
	if activeGatewayMode == desktop.GatewayModeClient && endpointIsThisHost(ctx, serverEndpoint) {
		// This desktop is a client of a gateway on this machine, so treat it as the host
		// when the default server profile holds the loopback administrator credentials.
		if _, err := storage.LoadLocalClientCredentials(gatewayStateDirectory); err == nil {
			hostedOnThisMachine = true
			generatePeeringKey = func(keyCtx context.Context, copyCurrent bool) (string, error) {
				return requestServerPeeringKey(keyCtx, gatewayStateDirectory, copyCurrent)
			}
		}
	}
	setupConfig := &desktop.GatewaySetupConfig{
		Preferences: gatewayPrefs, ActiveMode: activeGatewayMode, PreferencesPath: gatewayPreferencesPath,
		StateDirectory: gatewayStateDirectory, ServerEndpoint: serverEndpoint,
		GeneratePeeringKey: generatePeeringKey, HostedOnThisMachine: hostedOnThisMachine,
		Restart: func() {
			restartRequested = true
			view.Close()
			cancel()
			windowApp.Quit()
		},
	}
	setupConfig.Apply = func(changeCtx context.Context, prefs desktop.GatewayPreferences) (desktop.GatewayPreferences, error) {
		var hosted *config.HostedGateway
		enrollment, endpoint := prefs.EnrolledProfile, prefs.Endpoint
		if prefs.Mode == desktop.GatewayModeServer {
			var err error
			hosted, err = hostedGatewaySettings(&prefs)
			if err != nil {
				return prefs, err
			}
			enrollment = filepath.Join(gatewayStateDirectory, "local-client.json")
			endpoint = config.LocalGatewayEndpoint
		}
		if err := writeClientConfig(changeCtx, clientConfigPath, enrollment, endpoint, preferences, "", true, hosted); err != nil {
			return prefs, err
		}
		return prefs, config.LoadClient(clientConfigPath)
	}
	cliExecutable, err := desktopCLI()
	if err != nil {
		return err
	}
	view, err = desktop.NewBrowser(desktop.BrowserConfig{ClientConfigPath: clientConfigPath, Client: client, LocalConversationsDirectory: chatRoot, PreferencesPath: preferences, WorkPreferencesPath: workPreferencesPath, GatewaySetup: setupConfig, NewWindow: windowApp.NewWindow, NotifyGlobal: func(message string) {
		windowApp.SendNotification(fyne.NewNotification("Amux notification", message))
	}, AppControls: desktop.NewAppControlsPanel(cliExecutable, func(archive string) error {
		if err := scheduleDesktopUpdate(ctx, cliExecutable, clientConfigPath, client.Endpoint(), archive, args); err != nil {
			return err
		}
		fyne.Do(windowApp.Quit)
		return nil
	})})
	if err != nil {
		return err
	}

	window := windowApp.NewWindow("Amux")
	window.Resize(fyne.NewSize(1280, 820))
	window.SetContent(view.Content())
	window.SetCloseIntercept(func() {
		view.Close()
		cancel()
		window.Close()
	})
	view.Start(ctx)
	if failure := update.TakeFailure(); failure != "" {
		message := widget.NewLabel(failure)
		message.Wrapping = fyne.TextWrapWord
		var notice *widget.PopUp
		content := container.NewPadded(container.NewVBox(message, widget.NewButton("OK", func() { notice.Hide() })))
		notice = widget.NewModalPopUp(content, window.Canvas())
		notice.Resize(fyne.NewSize(480, content.MinSize().Height))
		notice.Show()
	}
	stopShutdown := context.AfterFunc(parent, func() { fyne.Do(windowApp.Quit) })
	defer stopShutdown()
	window.ShowAndRun()
	view.Close()
	cancel()
	view.Wait()
	if restartRequested {
		return errRestartDesktop
	}
	return nil
}
