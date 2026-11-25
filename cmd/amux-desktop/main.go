//go:build desktop

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"github.com/nitecon/amux/desktop"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/clientconfig"
	"github.com/nitecon/amux/internal/clientcontrol"
	"github.com/nitecon/amux/internal/gatewayprocess"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
	"net/http"
)

var errRestartDesktop = errors.New("restart desktop")

func main() {
	args := os.Args[1:]
	err := run(args)
	if errors.Is(err, errRestartDesktop) {
		err = relaunchDesktop(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amux-desktop:", err)
		os.Exit(1)
	}
}

// GLFW's desktop driver runs once per process. Relaunch only after run has
// released the previous gateway, worker journal, and other application state.
func relaunchDesktop(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate desktop for restart: %w", err)
	}
	if runtime.GOOS == "darwin" {
		macosDir := filepath.Dir(executable)
		contentsDir := filepath.Dir(macosDir)
		bundle := filepath.Dir(contentsDir)
		if filepath.Base(macosDir) == "MacOS" && filepath.Base(contentsDir) == "Contents" && filepath.Ext(bundle) == ".app" {
			launch := exec.Command("/usr/bin/open", append([]string{"-n", bundle, "--args"}, args...)...)
			if err := launch.Run(); err != nil {
				return fmt.Errorf("restart desktop bundle: %w", err)
			}
			return nil
		}
	}
	command := exec.Command(executable, args...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("restart desktop: %w", err)
	}
	_ = command.Process.Release()
	return nil
}

type desktopView interface {
	Content() fyne.CanvasObject
	Start(context.Context)
	Close()
	Wait()
}

func gatewayStatePath(configDirectory, preferencesPath, explicitState string, preferencesExplicit bool) (string, error) {
	stateDirectory := filepath.Join(configDirectory, "amux")
	if explicitState != "" {
		stateDirectory = explicitState
	} else if preferencesExplicit {
		stateDirectory = filepath.Join(filepath.Dir(preferencesPath), "gateway-profile")
	}
	return filepath.Abs(stateDirectory)
}

func run(args []string) error {
	flags := flag.NewFlagSet("amux-desktop", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	serverURL := flags.String("server", "", "connect to a gateway instead of starting the local server")
	database := flags.String("database", "", "local gateway database (OS config directory by default)")
	project := flags.String("project", "", "initial browser project, or project for a direct conversation")
	conversationID := flags.String("conversation", "", "open a conversation directly instead of browsing projects and tasks")
	preferences := flags.String("preferences", "", "navigation preferences file (OS config directory by default; --preferences= uses memory only)")
	workPreferences := flags.String("work-preferences", "", "work submission preferences file (OS config directory by default)")
	gatewayPreferencesPath := flags.String("gateway-preferences", "", "gateway mode preferences file (OS config directory by default)")
	gatewayStateDir := flags.String("gateway-state-dir", "", "server identity and enrolled peer directory (defaults beside --gateway-preferences when supplied)")
	keyEnv := flags.String("api-key-env", "GATEWAY_API_KEY", "environment variable containing the gateway API credential")
	enrolledProfile := flags.String("enrolled-profile", "", "enrolled gateway credential manifest")
	state := flags.String("state", "", "local worker journal (OS config directory by default in browser mode; --state= disables workers)")
	runID := flags.String("run-id", "", "resume this saved worker run alongside the chat")
	toolsCommand := flags.String("task-tools-command", "", "amux MCP executable (sibling or PATH amux by default for worker pools)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if (*serverURL != "" || *enrolledProfile != "") && *database != "" {
		return fmt.Errorf("--database cannot be combined with --server or --enrolled-profile")
	}
	if *conversationID != "" && *project == "" {
		return fmt.Errorf("--conversation requires --project")
	}
	configDirectory, configErr := os.UserConfigDir()
	if configErr != nil && *gatewayPreferencesPath == "" {
		return fmt.Errorf("locate desktop configuration: supply --gateway-preferences: %w", configErr)
	}
	if *gatewayPreferencesPath == "" {
		*gatewayPreferencesPath = filepath.Join(configDirectory, "amux", "gateway.json")
	}
	if configDirectory == "" {
		configDirectory = filepath.Dir(filepath.Dir(*gatewayPreferencesPath))
	}
	gatewayPreferencesExplicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "gateway-preferences" {
			gatewayPreferencesExplicit = true
		}
	})
	gatewayStateDirectory, pathErr := gatewayStatePath(configDirectory, *gatewayPreferencesPath, *gatewayStateDir, gatewayPreferencesExplicit)
	if pathErr != nil {
		return fmt.Errorf("resolve gateway state directory: %w", pathErr)
	}
	gatewayPrefs, gatewayPrefsErr := desktop.LoadGatewayPreferences(*gatewayPreferencesPath)
	if gatewayPrefsErr != nil {
		gatewayPrefs = desktop.DefaultGatewayPreferences()
	}
	var err error
	serverExplicit, profileExplicit, databaseExplicit := false, false, false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "server" {
			serverExplicit = true
		}
		if f.Name == "enrolled-profile" {
			profileExplicit = true
		}
		if f.Name == "database" {
			databaseExplicit = true
		}
	})
	connectionExplicit := serverExplicit || profileExplicit || databaseExplicit
	clientConfigPath := ""
	existingClientConfig := false
	var selectedHostedGateway *clientconfig.HostedGateway
	if !connectionExplicit && !gatewayPreferencesExplicit && *conversationID == "" {
		clientConfigPath, err = clientconfig.DefaultPath()
		if err != nil {
			return err
		}
		config, loadErr := clientconfig.Load(clientConfigPath)
		if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			probeCtx, stop := context.WithTimeout(context.Background(), time.Second)
			accepted, probeErr := clientcontrol.LoadProfile(probeCtx, clientConfigPath)
			stop()
			if probeErr == nil {
				config, loadErr = accepted, nil
			}
		}
		if loadErr == nil {
			*enrolledProfile = config.EnrolledProfile
			*serverURL = config.GatewayEndpoint
			if config.APIKeyEnv != "" {
				*keyEnv = config.APIKeyEnv
			}
			existingClientConfig = true
			selectedHostedGateway = config.HostedGateway
			gatewayPrefs.Mode = desktop.GatewayModeClient
			gatewayPrefs.Endpoint, gatewayPrefs.EnrolledProfile = *serverURL, *enrolledProfile
			if selectedHostedGateway != nil {
				gatewayPrefs.Mode = desktop.GatewayModeServer
				gatewayPrefs.RemoteHosting = selectedHostedGateway.RemoteHosting
				gatewayPrefs.AdvertisedHost = selectedHostedGateway.AdvertisedHost
				gatewayPrefs.ServerInstanceID = selectedHostedGateway.InstanceID
			}
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return fmt.Errorf("load local client configuration: %w", loadErr)
		}
	}
	if !existingClientConfig && gatewayPrefsErr != nil {
		return gatewayPrefsErr
	}
	if !existingClientConfig && !connectionExplicit && *conversationID == "" {
		if _, err := os.Stat(*gatewayPreferencesPath); errors.Is(err, os.ErrNotExist) {
			return showGatewayOnboarding(*gatewayPreferencesPath, gatewayStateDirectory)
		} else if err != nil {
			return err
		}
	}
	if !connectionExplicit && !existingClientConfig && gatewayPrefs.Mode == desktop.GatewayModeClient {
		*serverURL = gatewayPrefs.Endpoint
		*enrolledProfile = gatewayPrefs.EnrolledProfile
	}
	stateExplicit := false
	if *conversationID == "" {
		preferencesExplicit := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "preferences" {
				preferencesExplicit = true
			}
			if f.Name == "state" {
				stateExplicit = true
			}
		})
		if !preferencesExplicit || !stateExplicit {
			directory, err := os.UserConfigDir()
			if err != nil {
				return fmt.Errorf("locate desktop configuration: supply --preferences and --state paths, or empty values to disable persistence and workers")
			}
			if !preferencesExplicit {
				*preferences = filepath.Join(directory, "amux", "navigation.json")
			}
			if !stateExplicit {
				*state = filepath.Join(directory, "amux", "workers.db")
			}
		}
	}
	if *toolsCommand != "" && *state == "" {
		return fmt.Errorf("--task-tools-command requires a local worker journal")
	}
	configuredWorkPreferences := os.Getenv("AMUX_WORK_SUBMISSION_PREFERENCES")
	workPreferencesPath, err := worksubmission.OptionalPreferencesPath(*workPreferences, configuredWorkPreferences)
	if err != nil {
		return fmt.Errorf("resolve work submission preferences: %w", err)
	}
	key := os.Getenv(*keyEnv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managedClient := false
	attachClient := func(endpoint string, hosted *clientconfig.HostedGateway) error {
		journal := *state
		if !stateExplicit {
			if _, statErr := os.Stat(journal); errors.Is(statErr, os.ErrNotExist) {
				journal = ""
			}
		}
		if !existingClientConfig {
			if err := writeClientConfig(ctx, clientConfigPath, *enrolledProfile, endpoint, *preferences, journal, true, hosted); err != nil {
				return fmt.Errorf("configure local client: %w", err)
			}
		}
		cli, err := amuxCLIPath()
		if err != nil {
			return err
		}
		if !stateExplicit {
			journal = ""
		}
		if err := startOrAttachClient(ctx, cli, clientConfigPath, endpoint, journal); err != nil {
			return err
		}
		managedClient = true
		return nil
	}
	serverProfile, serverEndpoint := "", ""
	localGatewayStarted := false
	if !connectionExplicit && !existingClientConfig && gatewayPrefs.Mode == desktop.GatewayModeServer {
		if configDirectory == "" {
			return fmt.Errorf("secure gateway hosting requires a user configuration directory")
		}
		serverProfile = gatewayStateDirectory
		hosted, err := hostedGatewaySettings(&gatewayPrefs)
		if err != nil {
			return err
		}
		if err := desktop.SaveGatewayPreferences(*gatewayPreferencesPath, gatewayPrefs); err != nil {
			return err
		}
		advertisedHost := hosted.AdvertisedHost
		*serverURL = clientconfig.LocalGatewayEndpoint
		*enrolledProfile = filepath.Join(serverProfile, "local-client.json")
		serverEndpoint = "https://" + net.JoinHostPort(advertisedHost, "7913")
		if clientConfigPath != "" && *state != "" {
			if err := attachClient(*serverURL, hosted); err != nil {
				return err
			}
		} else {
			_, _, stop, err := startSecureGateway(ctx, serverProfile, gatewayPrefs.ServerInstanceID, advertisedHost, gatewayPrefs.RemoteHosting)
			if err != nil {
				return err
			}
			defer stop()
		}
	} else if selectedHostedGateway != nil && clientConfigPath != "" && *state != "" {
		serverProfile = filepath.Dir(*enrolledProfile)
		*serverURL = clientconfig.LocalGatewayEndpoint
		serverEndpoint = "https://" + net.JoinHostPort(selectedHostedGateway.AdvertisedHost, "7913")
		if err := attachClient(*serverURL, selectedHostedGateway); err != nil {
			return err
		}
	} else if *serverURL == "" && *enrolledProfile == "" {
		if key == "" {
			key = rand.Text()
			if err := os.Setenv(*keyEnv, key); err != nil {
				return fmt.Errorf("configure the local gateway credential: %w", err)
			}
		}
		if *database == "" {
			*database = filepath.Join(gatewayStateDirectory, "application.sqlite")
		}
		cli, err := amuxCLIPath()
		if err != nil {
			return err
		}
		startup, stopStartup := context.WithTimeout(ctx, 20*time.Second)
		endpoint, stop, err := gatewayprocess.StartHTTP(startup, cli, *database, "127.0.0.1:7913", *keyEnv, os.Stderr)
		stopStartup()
		if err != nil {
			return err
		}
		defer stop()
		*serverURL = endpoint
		serverEndpoint = endpoint
		localGatewayStarted = true
	}
	if *enrolledProfile == "" && key == "" {
		return fmt.Errorf("gateway API credential is not configured in %s", *keyEnv)
	}
	var client *gatewayclient.Client
	if *enrolledProfile != "" {
		*enrolledProfile, err = filepath.Abs(*enrolledProfile)
		if err == nil {
			client, err = gatewayclient.NewEnrolledFromManifest(*serverURL, *enrolledProfile)
		}
	} else {
		client, err = gatewayclient.New(*serverURL, key, nil)
	}
	if err != nil {
		return err
	}
	if serverEndpoint == "" {
		serverEndpoint = client.Endpoint()
	}
	if !managedClient && *conversationID == "" {
		if clientConfigPath != "" && (existingClientConfig || *enrolledProfile != "") && *state != "" && *toolsCommand == "" && *workPreferences == "" {
			if err := attachClient(client.Endpoint(), nil); err != nil {
				return err
			}
		} else {
			override := clientconfig.Config{
				EnrolledProfile: *enrolledProfile, GatewayEndpoint: client.Endpoint(),
				TaskToolsCommand: *toolsCommand, WorkPreferences: workPreferencesPath,
			}
			if *enrolledProfile == "" {
				override.APIKeyEnv = *keyEnv
			}
			if *state != "" {
				override.Providers = []string{"antigravity", "codex", "claude"}
				if stateExplicit {
					override.WorkerJournal, err = filepath.Abs(*state)
					if err != nil {
						return err
					}
				}
			}
			if *state == "" {
				override.Providers = []string{}
			}
			clientConfigPath, err = writeOverrideClient(ctx, gatewayStateDirectory, *preferences, override)
			if err != nil {
				return err
			}
			cli, err := amuxCLIPath()
			if err != nil {
				return err
			}
			if err := startOrAttachClient(ctx, cli, clientConfigPath, client.Endpoint(), ""); err != nil {
				return err
			}
			managedClient = true
			if localGatewayStarted || serverProfile != "" {
				defer func() {
					stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_, _ = clientcontrol.Request(stop, clientConfigPath, http.MethodPost, "/shutdown", nil)
				}()
			}
		}
	}
	activeGatewayMode := desktop.GatewayModeServer
	if serverProfile == "" && !localGatewayStarted && (*serverURL != "" || *enrolledProfile != "") {
		activeGatewayMode = desktop.GatewayModeClient
	}

	if *conversationID != "" && (*state == "") != (*runID == "") {
		return fmt.Errorf("--state and --run-id must be supplied together")
	}
	if *toolsCommand != "" && *runID == "" && *conversationID != "" {
		return fmt.Errorf("--task-tools-command requires a hosted --run-id")
	}
	if *runID != "" && *conversationID == "" {
		return fmt.Errorf("hosting a saved worker requires --project and --conversation")
	}

	var local *storage.Store
	var releaseProfile func()
	var workerDone chan error
	if *runID != "" {
		lock, lockErr := server.LockProfile(*state)
		if lockErr != nil {
			return lockErr
		}
		releaseProfile = func() { _ = lock.Close() }
		defer releaseProfile()
		local, err = storage.Open(ctx, *state)
		if err != nil {
			return err
		}
		defer local.Close()
	}
	var chatRoot string
	if *conversationID == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return fmt.Errorf("locate local conversation directory: %w", homeErr)
		}
		chatRoot = filepath.Join(home, ".amux", "conversations")
	}

	windowApp := app.New()
	windowApp.Settings().SetTheme(desktop.WorkspaceTheme())
	restartRequested := false
	stagedUpdate := ""
	var view desktopView
	var generatePeeringKey func(context.Context, bool) (string, error)
	if serverProfile != "" && (gatewayPrefs.RemoteHosting || selectedHostedGateway != nil && selectedHostedGateway.RemoteHosting) {
		generatePeeringKey = func(keyCtx context.Context, copyCurrent bool) (string, error) {
			return requestServerPeeringKey(keyCtx, serverProfile, copyCurrent)
		}
	}
	hostedOnThisMachine := false
	if activeGatewayMode == desktop.GatewayModeClient && serverProfile == "" && gatewayStateDirectory != "" && endpointIsThisHost(ctx, serverEndpoint) {
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
		Preferences: gatewayPrefs, ActiveMode: activeGatewayMode, PreferencesPath: *gatewayPreferencesPath,
		StateDirectory: gatewayStateDirectory, ServerEndpoint: serverEndpoint, ConnectionOverride: connectionExplicit,
		GeneratePeeringKey: generatePeeringKey, HostedOnThisMachine: hostedOnThisMachine,
		Restart: func() {
			restartRequested = true
			view.Close()
			cancel()
			windowApp.Quit()
		},
	}
	if managedClient && !connectionExplicit && !gatewayPreferencesExplicit && *state != "" {
		setupConfig.Apply = func(changeCtx context.Context, prefs desktop.GatewayPreferences) (desktop.GatewayPreferences, error) {
			var hosted *clientconfig.HostedGateway
			enrollment, endpoint := prefs.EnrolledProfile, prefs.Endpoint
			if prefs.Mode == desktop.GatewayModeServer {
				var err error
				hosted, err = hostedGatewaySettings(&prefs)
				if err != nil {
					return prefs, err
				}
				enrollment = filepath.Join(gatewayStateDirectory, "local-client.json")
				endpoint = clientconfig.LocalGatewayEndpoint
			}
			err := writeClientConfig(changeCtx, clientConfigPath, enrollment, endpoint, *preferences, "", true, hosted)
			return prefs, err
		}
	}
	cliExecutable, err := desktopCLI()
	if err != nil {
		return err
	}
	if *conversationID != "" {
		view, err = desktop.New(desktop.Config{Client: client, Project: *project, ConversationID: *conversationID})
	} else {
		managedConfigPath := ""
		if managedClient {
			managedConfigPath = clientConfigPath
		}
		view, err = desktop.NewBrowser(desktop.BrowserConfig{ClientConfigPath: managedConfigPath, Client: client, LocalConversationsDirectory: chatRoot, Project: *project, PreferencesPath: *preferences, WorkPreferencesPath: workPreferencesPath, GatewaySetup: setupConfig, NewWindow: windowApp.NewWindow, AppControls: desktop.NewAppControlsPanel(cliExecutable, func(archive string) {
			stagedUpdate = archive
			windowApp.Quit()
		})})
	}
	if err != nil {
		return err
	}
	if *runID != "" {
		saved, loadErr := local.WorkerRun(ctx, *runID)
		if loadErr != nil {
			return loadErr
		}
		if saved.Server != client.Endpoint() || saved.Project != *project || saved.ConversationID != *conversationID {
			return fmt.Errorf("saved worker does not match --server, --project and --conversation")
		}
		if err := local.Close(); err != nil {
			return err
		}
		releaseProfile()
		cli, err := amuxCLIPath()
		if err != nil {
			return err
		}
		workerArgs := []string{"worker", "run", "--state", *state, "--run-id", *runID, "--server", client.Endpoint(), "--api-key-env", *keyEnv}
		if *enrolledProfile != "" {
			workerArgs = append(workerArgs, "--enrolled-profile", *enrolledProfile)
		}
		if *toolsCommand != "" {
			workerArgs = append(workerArgs, "--task-tools-command", *toolsCommand)
		}
		command := exec.CommandContext(ctx, cli, workerArgs...)
		hideClientWindow(command)
		command.Cancel = func() error {
			if err := command.Process.Signal(os.Interrupt); err != nil {
				return command.Process.Kill()
			}
			return nil
		}
		command.WaitDelay = 8 * time.Second
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if workPreferencesPath != "" {
			command.Env = append(os.Environ(), "AMUX_WORK_SUBMISSION_PREFERENCES="+workPreferencesPath)
		}
		if err := command.Start(); err != nil {
			return fmt.Errorf("start saved worker: %w", err)
		}
		workerDone = make(chan error, 1)
		go func() {
			err := command.Wait()
			if ctx.Err() != nil {
				err = nil
			}
			workerDone <- err
		}()
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
	window.ShowAndRun()
	view.Close()
	cancel()
	view.Wait()
	if workerDone != nil {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case workerErr := <-workerDone:
			if workerErr != nil {
				return workerErr
			}
		case <-timer.C:
			return fmt.Errorf("saved worker did not stop within 10 seconds")
		}
	}
	if stagedUpdate != "" {
		args := []string{"--apply-update", stagedUpdate, "--restart-desktop", "--wait-pid", strconv.Itoa(os.Getpid()), "--"}
		args = append(args, os.Args[1:]...)
		command := exec.Command(cliExecutable, args...)
		if err := command.Start(); err != nil {
			return fmt.Errorf("could not schedule verified update: %w", err)
		}
		return nil
	}
	if restartRequested {
		return errRestartDesktop
	}
	return nil
}
