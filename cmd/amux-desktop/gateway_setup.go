//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nitecon/amux/internal/gatewayprocess"
)

func amuxCLIPath() (string, error) {
	if candidate, err := desktopCLI(); err == nil {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	if path, err := exec.LookPath("amux"); err == nil {
		return path, nil
	}
	return "", errors.New("the amux command-line program is required for secure desktop gateway hosting")
}

func startSecureGateway(ctx context.Context, profile, instanceID, advertisedHost string, remoteHosting bool) (string, string, func(), error) {
	cli, err := amuxCLIPath()
	if err != nil {
		return "", "", nil, err
	}
	return gatewayprocess.Start(ctx, cli, profile, instanceID, advertisedHost, remoteHosting, os.Stderr)
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
