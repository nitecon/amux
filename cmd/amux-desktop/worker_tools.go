package main

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

// Resolve lazily so a missing task-tools installation does not prevent browsing.
func desktopWorkerOpen(command, endpoint, keyEnv string) worker.Open {
	return desktopWorkerOpenWithPreferences(command, endpoint, keyEnv, "")
}

func desktopWorkerOpenWithPreferences(command, endpoint, keyEnv, workPreferencesPath string) worker.Open {
	return desktopWorkerOpenWithEnrolledProfile(command, endpoint, keyEnv, workPreferencesPath, "")
}

func desktopWorkerOpenWithEnrolledProfile(command, endpoint, keyEnv, workPreferencesPath, enrolledProfile string) worker.Open {
	return func(ctx context.Context, run storage.WorkerRun, sink conversation.Sink) (worker.Session, error) {
		executable, err := resolveTaskTools(command)
		if err != nil {
			return nil, err
		}
		return worker.OpenWithNativeTools(worker.NativeToolsConfig{Executable: executable, Server: endpoint, APIKeyEnv: keyEnv, EnrolledProfile: enrolledProfile, WorkPreferencesPath: workPreferencesPath})(ctx, run, sink)
	}
}

func resolveTaskTools(command string) (string, error) {
	if command != "" {
		resolved, err := exec.LookPath(command)
		if err != nil {
			return "", fmt.Errorf("locate amux task tools: %w", err)
		}
		return filepath.Abs(resolved)
	}
	name := "amux"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		candidates := []string{filepath.Join(dir, name)}
		if runtime.GOOS == "darwin" {
			candidates = append(candidates, filepath.Join(dir, "amux-macos-"+runtime.GOARCH))
		}
		for _, candidate := range candidates {
			resolved, err := exec.LookPath(candidate)
			if err != nil {
				continue
			}
			if runtime.GOOS == "darwin" {
				if linuxBinary, err := elf.Open(resolved); err == nil {
					_ = linuxBinary.Close()
					continue
				}
			}
			return resolved, nil
		}
	}
	if resolved, err := exec.LookPath(name); err == nil {
		return filepath.Abs(resolved)
	}
	return "", fmt.Errorf("amux task tools are unavailable: install amux beside amux-desktop or on PATH, or supply --task-tools-command")
}
