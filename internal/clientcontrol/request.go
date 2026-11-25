// Package clientcontrol connects desktop and CLI callers to the local Amux client.
package clientcontrol

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func Request(ctx context.Context, config, method, route string, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := Open(ctx, config, method, route, payload)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(io.LimitReader(response.Body, 1<<20))
}

var localHTTP = &http.Client{
	Transport:     &http.Transport{},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Open starts an authenticated request whose lifetime is controlled by ctx.
func Open(ctx context.Context, config, method, route string, payload []byte) (*http.Response, error) {
	data, err := os.ReadFile(config + ".control")
	if err != nil {
		return nil, fmt.Errorf("read running client address: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return nil, fmt.Errorf("invalid client control address")
	}
	host, _, err := net.SplitHostPort(fields[0])
	if err != nil || host != "127.0.0.1" {
		return nil, fmt.Errorf("client control address must be loopback")
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+fields[0]+route, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+fields[1])
	response, err := localHTTP.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 65536))
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("client control: %s", strings.TrimSpace(string(body)))
	}
	return response, nil
}

// CheckoutChange updates only the checkout setting edited by the desktop.
type CheckoutChange struct {
	ProjectsDirectory *string `json:"projects_directory,omitempty"`
	Project           string  `json:"project,omitempty"`
	Checkout          string  `json:"checkout,omitempty"`
	Recreate          bool    `json:"recreate,omitempty"`
}
