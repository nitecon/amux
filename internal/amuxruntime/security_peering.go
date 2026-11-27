package amuxruntime

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nitecon/amux/storage"
	"github.com/urfave/cli/v3"
)

func runServerPeeringKey(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := commandFlags("amux server peering-key", stderr)
	profile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "profile", Value: "", Usage: "server profile directory (defaults to the desktop server profile in the OS config directory)", Destination: profile})
	copyCurrent := new(bool)
	flags.Flags = append(flags.Flags, &cli.BoolFlag{Name: "copy", Value: false, Usage: "print the active key without replacing or extending it", Destination: copyCurrent})
	if err := parseCommand(ctx, flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("usage: amux server peering-key [--profile DIR] [--copy]")
	}
	if strings.TrimSpace(*profile) == "" {
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate default server profile: supply --profile: %w", err)
		}
		*profile = filepath.Join(configDirectory, "amux")
	}
	key, err := ServerPeeringKey(ctx, *profile, *copyCurrent)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, key)
	return err
}

func ServerPeeringKey(ctx context.Context, profile string, copyCurrent bool) (string, error) {
	credentials, err := storage.LoadLocalClientCredentials(profile)
	if err != nil {
		return "", err
	}
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: credentials.RootCAs,
			Certificates: []tls.Certificate{credentials.Certificate},
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer transport.CloseIdleConnections()
	method := http.MethodPost
	if copyCurrent {
		method = http.MethodGet
	}
	endpoint := strings.TrimSuffix(credentials.Profile.URL, "/") + "/v1/server/invitation"
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("prepare local server request")
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("local server invitation request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("local server invitation request failed with HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var invitation storage.Invitation
	if err := decoder.Decode(&invitation); err != nil || invitation.Invitation == "" {
		return "", fmt.Errorf("local server returned an invalid invitation")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("local server returned an invalid invitation")
	}
	return invitation.Invitation, nil
}
