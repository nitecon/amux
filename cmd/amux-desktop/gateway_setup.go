//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

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

// endpointIsThisHost reports whether a gateway endpoint names this machine:
// a loopback address, the system hostname, or an address bound to a local
// interface.
func endpointIsThisHost(ctx context.Context, endpoint string) bool {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" {
		return true
	}
	var candidates []net.IP
	if ip := net.ParseIP(host); ip != nil {
		candidates = []net.IP{ip}
	} else {
		// Match the hostname by its first label so "box" and "box.lan" agree.
		if name, err := os.Hostname(); err == nil && strings.SplitN(host, ".", 2)[0] == strings.SplitN(strings.ToLower(name), ".", 2)[0] {
			return true
		}
		lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(lookup, host)
		if err != nil {
			return false
		}
		for _, addr := range addrs {
			candidates = append(candidates, addr.IP)
		}
	}
	local, _ := net.InterfaceAddrs()
	for _, ip := range candidates {
		if ip.IsLoopback() {
			return true
		}
		for _, addr := range local {
			if network, ok := addr.(*net.IPNet); ok && network.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}
