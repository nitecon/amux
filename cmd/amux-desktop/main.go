//go:build desktop

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"github.com/google/uuid"
	"github.com/nitecon/amux/desktop"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
	"github.com/nitecon/amux/worksubmission"
)

var errRestartDesktop = errors.New("restart desktop")

func main() {
	for {
		err := run(os.Args[1:])
		if errors.Is(err, errRestartDesktop) {
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "amux-desktop:", err)
			os.Exit(1)
		}
		return
	}
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
	gatewayPrefs, err := desktop.LoadGatewayPreferences(*gatewayPreferencesPath)
	if err != nil {
		return err
	}
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
	if !connectionExplicit && gatewayPrefs.Mode == desktop.GatewayModeClient {
		*serverURL = gatewayPrefs.Endpoint
		*enrolledProfile = gatewayPrefs.EnrolledProfile
	}
	key := os.Getenv(*keyEnv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverProfile, serverEndpoint := "", ""
	localGatewayStarted := false
	if !connectionExplicit && gatewayPrefs.Mode == desktop.GatewayModeServer && gatewayPrefs.RemoteHosting {
		if configDirectory == "" {
			return fmt.Errorf("secure gateway hosting requires a user configuration directory")
		}
		serverProfile = gatewayStateDirectory
		advertisedHost, err := desktop.NormalizeAdvertisedHost(gatewayPrefs.AdvertisedHost)
		if err != nil {
			return err
		}
		if gatewayPrefs.ServerInstanceID == "" {
			instanceID, err := uuid.NewV7()
			if err != nil {
				return err
			}
			gatewayPrefs.ServerInstanceID = instanceID.String()
			if err := desktop.SaveGatewayPreferences(*gatewayPreferencesPath, gatewayPrefs); err != nil {
				return err
			}
		}
		localEndpoint, publicEndpoint, stop, err := startSecureGateway(ctx, serverProfile, gatewayPrefs.ServerInstanceID, advertisedHost)
		if err != nil {
			return err
		}
		defer stop()
		*serverURL = localEndpoint
		*enrolledProfile = filepath.Join(serverProfile, "local-client.json")
		serverEndpoint = publicEndpoint
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
		endpoint, stop, err := startLocalGateway(ctx, *database, "127.0.0.1:7913", key)
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
	activeGatewayMode := desktop.GatewayModeServer
	if serverProfile == "" && !localGatewayStarted && (*serverURL != "" || *enrolledProfile != "") {
		activeGatewayMode = desktop.GatewayModeClient
	}
	configuredWorkPreferences := os.Getenv("AMUX_WORK_SUBMISSION_PREFERENCES")
	workPreferencesPath, err := worksubmission.OptionalPreferencesPath(*workPreferences, configuredWorkPreferences)
	if err != nil {
		return fmt.Errorf("resolve work submission preferences: %w", err)
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
	var local *storage.Store
	var releaseProfile func()
	var workerDone chan error
	if *state != "" {
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
	var manager *desktop.WorkerManager
	if *conversationID == "" && local != nil {
		conversationRoot := filepath.Dir(*state)
		if !stateExplicit && runtime.GOOS != "windows" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("locate project conversation directory: %w", err)
			}
			conversationRoot = filepath.Join(home, ".amux")
		}
		manager, err = desktop.NewWorkerManager(desktop.WorkerManagerConfig{
			Store: local, Client: client, ConversationRoot: conversationRoot,
			Open: desktopWorkerOpenWithEnrolledProfile(*toolsCommand, client.Endpoint(), *keyEnv, workPreferencesPath, *enrolledProfile),
		})
		if err != nil {
			return err
		}
		defer func() {
			manager.Close()
			manager.Wait()
		}()
	}
	windowApp := app.New()
	windowApp.Settings().SetTheme(desktop.WorkspaceTheme())
	restartRequested := false
	stagedUpdate := ""
	var generatePeeringKey func(context.Context, bool) (string, error)
	if serverProfile != "" {
		generatePeeringKey = func(keyCtx context.Context, copyCurrent bool) (string, error) {
			return requestServerPeeringKey(keyCtx, serverProfile, copyCurrent)
		}
	}
	setupConfig := &desktop.GatewaySetupConfig{
		Preferences: gatewayPrefs, ActiveMode: activeGatewayMode, PreferencesPath: *gatewayPreferencesPath,
		StateDirectory: gatewayStateDirectory, ServerEndpoint: serverEndpoint, ConnectionOverride: connectionExplicit,
		GeneratePeeringKey: generatePeeringKey,
		Restart: func() {
			restartRequested = true
			windowApp.Quit()
		},
	}
	var view desktopView
	desktopExecutable, err := os.Executable()
	if err != nil {
		return err
	}
	cliName := "amux"
	if runtime.GOOS == "windows" {
		cliName += ".exe"
	}
	cliExecutable := filepath.Join(filepath.Dir(desktopExecutable), cliName)
	if *conversationID != "" {
		view, err = desktop.New(desktop.Config{Client: client, Project: *project, ConversationID: *conversationID})
	} else {
		view, err = desktop.NewBrowser(desktop.BrowserConfig{Client: client, Project: *project, PreferencesPath: *preferences, WorkPreferencesPath: workPreferencesPath, GatewaySetup: setupConfig, WorkerManager: manager, NewWindow: windowApp.NewWindow, AppControls: desktop.NewAppControlsPanel(cliExecutable, func(archive string) {
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
		workerDone = make(chan error, 1)
		var open worker.Open
		if *toolsCommand != "" {
			open = desktopWorkerOpenWithEnrolledProfile(*toolsCommand, client.Endpoint(), *keyEnv, workPreferencesPath, *enrolledProfile)
		}
		go func() {
			workerDone <- worker.Run(ctx, worker.Config{Store: local, Client: client, RunID: *runID, Open: open, AdvertisePresence: true})
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
	view.Wait()
	cancel()
	if workerDone != nil {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case workerErr := <-workerDone:
			if workerErr != nil && !errors.Is(workerErr, context.Canceled) {
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
