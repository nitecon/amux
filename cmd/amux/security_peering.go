package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nitecon/amux/storage"
)

func runServerPeeringKey(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("amux server peering-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("profile", "", "server profile directory (required)")
	copyCurrent := flags.Bool("copy", false, "print the active key without replacing or extending it")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*profile) == "" {
		return fmt.Errorf("usage: amux server peering-key --profile DIR [--copy]")
	}
	credentials, err := storage.LoadLocalClientCredentials(*profile)
	if err != nil {
		return err
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
	if *copyCurrent {
		method = http.MethodGet
	}
	endpoint := strings.TrimSuffix(credentials.Profile.URL, "/") + "/v1/server/invitation"
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return fmt.Errorf("prepare local server request")
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("local server invitation request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("local server invitation request failed with HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var invitation storage.Invitation
	if err := decoder.Decode(&invitation); err != nil || invitation.Invitation == "" {
		return fmt.Errorf("local server returned an invalid invitation")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("local server returned an invalid invitation")
	}
	_, err = fmt.Fprintln(stdout, invitation.Invitation)
	return err
}
