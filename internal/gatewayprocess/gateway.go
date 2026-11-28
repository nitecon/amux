// Package gatewayprocess starts the standalone local Amux gateway.
package gatewayprocess

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nitecon/amux/config"
)

func Start(ctx context.Context, cli, profile, instanceID, address string, remoteHosting bool, stderr io.Writer) (localEndpoint, publicEndpoint string, stop func(), err error) {
	settings := config.DefaultServer()
	settings.Profile = profile
	settings.SecureInstance = instanceID
	settings.ProfileTLSNames = []string{address}
	if remoteHosting {
		settings.Listen = "0.0.0.0:7913"
	} else {
		settings.DiscoveryBind = "127.0.0.1:0"
	}
	_, stop, err = start(ctx, cli, settings, stderr)
	if err != nil {
		return "", "", nil, err
	}
	return "https://127.0.0.1:7913", "https://" + net.JoinHostPort(address, "7913"), stop, nil
}

func StartHTTP(ctx context.Context, cli, database, address, keyEnv string, stderr io.Writer) (string, func(), error) {
	settings := config.DefaultServer()
	settings.Database = database
	settings.Listen = address
	settings.KeyEnv = keyEnv
	bound, stop, err := start(ctx, cli, settings, stderr)
	if err != nil {
		return "", nil, err
	}
	return "http://" + bound, stop, nil
}

type startupResult struct {
	address string
	err     error
}

func start(ctx context.Context, cli string, settings config.Server, stderr io.Writer) (string, func(), error) {
	dir, err := os.MkdirTemp("", "amux-gateway-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "server.yml")
	if err := config.SaveServer(path, settings); err != nil {
		return "", nil, fmt.Errorf("write local gateway configuration: %w", err)
	}
	childCtx, cancelChild := context.WithCancel(context.Background())
	command := exec.CommandContext(childCtx, cli, "server", "--config", path)
	prepareProcess(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancelChild()
		return "", nil, err
	}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		cancelChild()
		return "", nil, fmt.Errorf("start local gateway: %w", err)
	}
	done := make(chan error, 1)
	started := make(chan startupResult, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready := false
		for scanner.Scan() {
			if !ready && strings.HasPrefix(scanner.Text(), "listening ") {
				ready = true
				started <- startupResult{address: strings.TrimPrefix(scanner.Text(), "listening ")}
			}
		}
		if !ready {
			if scanErr := scanner.Err(); scanErr != nil {
				started <- startupResult{err: scanErr}
			} else {
				started <- startupResult{err: errors.New("local gateway stopped before it was ready")}
			}
		}
		done <- command.Wait()
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if command.Process != nil {
				if err := command.Process.Signal(os.Interrupt); err != nil {
					cancelChild()
				}
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
	case result := <-started:
		if result.err != nil {
			stop()
			return "", nil, result.err
		}
		return result.address, stop, nil
	case processErr := <-done:
		cancelChild()
		if processErr == nil {
			processErr = errors.New("secure gateway exited before it was ready")
		}
		return "", nil, processErr
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
}
