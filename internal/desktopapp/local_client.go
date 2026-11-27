//go:build desktop

package desktopapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/update"
	"github.com/nitecon/amux/desktop"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/internal/clientcontrol"
	"github.com/nitecon/amux/server"
)

func desktopCLI() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(executable)
	if runtime.GOOS == "windows" {
		return filepath.Join(directory, "amux.exe"), nil
	}
	packaged := filepath.Join(directory, "amux")
	if runtime.GOOS == "linux" {
		suffixed := filepath.Join(directory, "amux.bin")
		if filepath.Ext(executable) == ".bin" {
			if _, err := exec.LookPath(suffixed); err == nil {
				return suffixed, nil
			}
		}
		if _, err := exec.LookPath(packaged); err != nil {
			if _, err := exec.LookPath(suffixed); err == nil {
				return suffixed, nil
			}
		}
	}
	return packaged, nil
}

func scheduleDesktopUpdate(ctx context.Context, cli, settings, endpoint, archive string, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Refuse before stopping the client, so a refused update disturbs nothing.
	if err := update.CheckInstallable(cli); err != nil {
		return fmt.Errorf("schedule update: %w", err)
	}
	if settings != "" {
		if err := stopClientForUpdate(ctx, settings); err != nil {
			return err
		}
	}
	if err := update.StartApply(cli, archive, nil, append([]string{}, args...)); err != nil {
		if settings != "" {
			clientCLI, resolveErr := amuxCLIPath()
			if resolveErr == nil {
				resolveErr = startOrAttachClient(ctx, clientCLI, settings, endpoint, "")
			}
			if resolveErr != nil {
				return fmt.Errorf("schedule update: %w; restart local client: %v", err, resolveErr)
			}
		}
		return fmt.Errorf("schedule update: %w", err)
	}
	return nil
}

func stopClientForUpdate(ctx context.Context, settings string) error {
	if lock, err := server.LockProfile(settings); err == nil {
		return lock.Close()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := clientcontrol.Request(ctx, settings, http.MethodPost, "/shutdown", nil); err != nil {
		return fmt.Errorf("stop local client for update: %w", err)
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if lock, err := server.LockProfile(settings); err == nil {
			return lock.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for local client to stop: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// Switch an existing client only when the desktop's gateway selection changed.
func writeClientConfig(ctx context.Context, path, enrollment, endpoint, preferences, journal string, switchGateway bool, hosted *config.HostedGateway) error {
	existing, err := config.ReadClient(path)
	hadConfig := err == nil
	switch {
	case err == nil:
		if filepath.Clean(existing.EnrolledProfile) == filepath.Clean(enrollment) && config.SameHostedGateway(existing.HostedGateway, hosted) {
			return nil
		}
		if !switchGateway {
			return fmt.Errorf("local client configuration refers to a different enrollment profile")
		}
		if existing.WorkerJournal != "" {
			journal = existing.WorkerJournal
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if journal != "" {
		journal, err = filepath.Abs(journal)
		if err != nil {
			return err
		}
	}
	settings := config.Client{EnrolledProfile: enrollment, HostedGateway: hosted, WorkerJournal: journal, Providers: []string{"antigravity", "codex", "claude"}}
	if hadConfig {
		settings.Providers = existing.Providers
		settings.ProjectsDirectory = existing.ProjectsDirectory
	}
	if preferences != "" {
		prefs, err := desktop.LoadNavigationPreferences(preferences)
		if err != nil {
			return err
		}
		directory, bindings := prefs.ClientCheckoutSettings(endpoint)
		if filepath.Base(enrollment) == "local-client.json" && endpoint == "https://127.0.0.1:7913" {
			oldDirectory, oldBindings := prefs.ClientCheckoutSettings("http://127.0.0.1:7913")
			if directory == "" {
				directory = oldDirectory
			}
			for project, checkout := range oldBindings {
				if bindings[project] == "" {
					bindings[project] = checkout
				}
			}
		}
		if !hadConfig {
			settings.ProjectsDirectory = directory
		}
		settings.Projects = bindings
	}
	if filepath.Clean(existing.EnrolledProfile) == filepath.Clean(enrollment) {
		settings.Projects = existing.Projects
		settings.ProjectURLs = existing.ProjectURLs
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	lock, lockErr := server.LockProfile(path)
	if lockErr != nil && switchGateway && hadConfig {
		waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := clientcontrol.Request(waitCtx, path, http.MethodPost, "/shutdown", nil); err != nil {
			return fmt.Errorf("stop previous local client: %w", err)
		}
		for lockErr != nil {
			select {
			case <-waitCtx.Done():
				return fmt.Errorf("waiting for previous local client: %w", waitCtx.Err())
			case <-time.After(100 * time.Millisecond):
				lock, lockErr = server.LockProfile(path)
			}
		}
	}
	if lockErr != nil {
		return lockErr
	}
	defer lock.Close()
	if latest, err := config.ReadClient(path); err == nil {
		if filepath.Clean(latest.EnrolledProfile) == filepath.Clean(enrollment) && config.SameHostedGateway(latest.HostedGateway, hosted) {
			return nil
		}
		if !switchGateway || latest.EnrolledProfile != existing.EnrolledProfile || !config.SameHostedGateway(latest.HostedGateway, existing.HostedGateway) {
			return fmt.Errorf("local client configuration changed during gateway selection")
		}
		settings.ExcludedProjects = latest.ExcludedProjects
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return config.SaveClient(path, settings)
}

func startOrAttachClient(ctx context.Context, cli, path, endpoint, journal string) error {
	expected, err := clientcontrol.ExecutableDigest(cli)
	if err != nil {
		return fmt.Errorf("identify companion amux binary: %w", err)
	}
	probe := func() (reachable, matches bool, err error) {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		response, err := clientcontrol.Open(probeCtx, path, http.MethodGet, "/status", nil)
		if err != nil {
			return false, false, nil
		}
		defer response.Body.Close()
		status, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			return false, false, err
		}
		if got := strings.TrimSpace(string(status)); got != endpoint {
			return true, false, fmt.Errorf("local client is attached to %q instead of %q", got, endpoint)
		}
		return true, response.Header.Get(clientcontrol.BuildHeader) == expected, nil
	}
	reachable, matches, err := probe()
	if err != nil || matches {
		return err
	}
	if reachable {
		restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := clientcontrol.Request(restartCtx, path, http.MethodPost, "/shutdown", nil); err != nil {
			return fmt.Errorf("restart outdated local client: %w", err)
		}
		for {
			lock, err := server.LockProfile(path)
			if err == nil {
				_ = lock.Close()
				break
			}
			if _, matches, err := probe(); err != nil || matches {
				return err
			}
			select {
			case <-restartCtx.Done():
				return fmt.Errorf("wait for outdated local client to stop: %w", restartCtx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	args := []string{"headless", "--config", path}
	if journal != "" {
		args = append(args, "--state", journal)
	}
	command := exec.Command(cli, args...)
	hideClientWindow(command)
	log, err := os.OpenFile(filepath.Join(filepath.Dir(path), "client.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		return fmt.Errorf("start local client: %w", err)
	}
	go command.Wait()

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("local client did not become ready; see %s", log.Name())
		case <-tick.C:
			if reachable, matches, err := probe(); err != nil || matches {
				return err
			} else if reachable {
				return fmt.Errorf("local client started with a different build; see %s", log.Name())
			}
		}
	}
}

// Explicit connection options get their own profile and journal. They can
// coexist with the default desktop or a headless service.
func writeOverrideClient(ctx context.Context, root, preferences string, settings config.Client) (string, error) {
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	key := sha256.Sum256(encoded)
	path := filepath.Join(root, "client-overrides", hex.EncodeToString(key[:16])+".yml")
	if settings.WorkerJournal == "" {
		settings.WorkerJournal = path + ".workers.db"
	}
	if _, err := config.ReadClient(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if preferences != "" {
		prefs, err := desktop.LoadNavigationPreferences(preferences)
		if err != nil {
			return "", err
		}
		settings.ProjectsDirectory, settings.Projects = prefs.ClientCheckoutSettings(settings.GatewayEndpoint)
	}
	lock, err := server.LockProfile(path)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := config.ReadClient(path); errors.Is(err, os.ErrNotExist) {
		return path, config.SaveClient(path, settings)
	} else {
		return path, err
	}
}

func hostedGatewaySettings(prefs *desktop.GatewayPreferences) (*config.HostedGateway, error) {
	host := "127.0.0.1"
	if prefs.RemoteHosting {
		var err error
		host, err = desktop.NormalizeAdvertisedHost(prefs.AdvertisedHost)
		if err != nil {
			return nil, err
		}
	}
	if prefs.ServerInstanceID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		prefs.ServerInstanceID = id.String()
	}
	return &config.HostedGateway{InstanceID: prefs.ServerInstanceID, AdvertisedHost: host, RemoteHosting: prefs.RemoteHosting}, nil
}
