//go:build desktop

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nitecon/amux/desktop"
)

func amuxCLIPath() (string, error) {
	if executable, err := os.Executable(); err == nil {
		name := "amux"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		candidate := filepath.Join(filepath.Dir(executable), name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath("amux"); err == nil {
		return path, nil
	}
	return "", errors.New("the amux command-line program is required for secure desktop gateway hosting")
}

func startSecureGateway(ctx context.Context, profile, instanceID, advertisedHost string) (localEndpoint, publicEndpoint string, stop func(), err error) {
	cli, err := amuxCLIPath()
	if err != nil {
		return "", "", nil, err
	}
	address, err := desktop.NormalizeAdvertisedHost(advertisedHost)
	if err != nil {
		return "", "", nil, err
	}
	args := []string{"server", "run", "--profile", profile, "--secure-instance-id", instanceID,
		"--listen", "0.0.0.0:7913", "--profile-tls-name", address}
	childCtx, cancelChild := context.WithCancel(context.Background())
	command := exec.CommandContext(childCtx, cli, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancelChild()
		return "", "", nil, err
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		cancelChild()
		return "", "", nil, fmt.Errorf("start secure gateway: %w", err)
	}
	done := make(chan error, 1)
	started := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready := false
		for scanner.Scan() {
			if !ready && strings.HasPrefix(scanner.Text(), "listening ") {
				ready = true
				started <- nil
			}
		}
		if !ready {
			if scanErr := scanner.Err(); scanErr != nil {
				started <- scanErr
			} else {
				started <- errors.New("secure gateway stopped before it was ready")
			}
		}
		done <- command.Wait()
	}()
	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() {
			if command.Process != nil {
				_ = command.Process.Signal(os.Interrupt)
			}
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				cancelChild()
				if command.Process != nil {
					_ = command.Process.Kill()
				}
				<-done
			}
			cancelChild()
		})
	}
	select {
	case startErr := <-started:
		if startErr != nil {
			stop()
			return "", "", nil, startErr
		}
	case processErr := <-done:
		if processErr == nil {
			processErr = errors.New("secure gateway exited before it was ready")
		}
		return "", "", nil, processErr
	case <-ctx.Done():
		stop()
		return "", "", nil, ctx.Err()
	}
	return "https://127.0.0.1:7913", "https://" + net.JoinHostPort(address, "7913"), stop, nil
}

func requestServerPeeringKey(ctx context.Context, profile string, copyCurrent bool) (string, error) {
	cli, err := amuxCLIPath()
	if err != nil {
		return "", err
	}
	args := []string{"server", "peering-key", "--profile", profile}
	if copyCurrent {
		args = append(args, "--copy")
	}
	output, err := exec.CommandContext(ctx, cli, args...).Output()
	if err != nil {
		return "", fmt.Errorf("request server peering key: %w", err)
	}
	key := strings.TrimSpace(string(output))
	if key == "" {
		return "", errors.New("server returned an empty peering key")
	}
	return key, nil
}
