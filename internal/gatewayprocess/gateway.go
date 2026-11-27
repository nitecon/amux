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
	"strings"
	"sync"
	"time"
)

func Start(ctx context.Context, cli, profile, instanceID, address string, remoteHosting bool, stderr io.Writer) (localEndpoint, publicEndpoint string, stop func(), err error) {
	listen := "127.0.0.1:7913"
	if remoteHosting {
		listen = "0.0.0.0:7913"
	}
	args := []string{"server", "--profile", profile, "--secure-instance-id", instanceID,
		"--listen", listen, "--profile-tls-name", address}
	if !remoteHosting {
		args = append(args, "--discovery-bind", "127.0.0.1:0")
	}
	_, stop, err = start(ctx, cli, args, stderr)
	if err != nil {
		return "", "", nil, err
	}
	return "https://127.0.0.1:7913", "https://" + net.JoinHostPort(address, "7913"), stop, nil
}

func StartHTTP(ctx context.Context, cli, database, address, keyEnv string, stderr io.Writer) (string, func(), error) {
	args := []string{"server", "--database", database, "--listen", address, "--api-key-env", keyEnv}
	bound, stop, err := start(ctx, cli, args, stderr)
	if err != nil {
		return "", nil, err
	}
	return "http://" + bound, stop, nil
}

type startupResult struct {
	address string
	err     error
}

func start(ctx context.Context, cli string, args []string, stderr io.Writer) (string, func(), error) {
	childCtx, cancelChild := context.WithCancel(context.Background())
	command := exec.CommandContext(childCtx, cli, args...)
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
