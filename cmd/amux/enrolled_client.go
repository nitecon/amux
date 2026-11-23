package main

import (
	"fmt"
	"path/filepath"

	"github.com/nitecon/amux/gatewayclient"
)

// gatewayClientForProfile selects one authentication mode. An enrolled
// profile option names a credential manifest directly; bearer credentials are
// never combined with it.
func gatewayClientForProfile(endpoint, manifestPath, apiKey string) (*gatewayclient.Client, string, error) {
	if manifestPath == "" {
		if endpoint == "" {
			return nil, "", fmt.Errorf("gateway server is required")
		}
		if apiKey == "" {
			return nil, "", fmt.Errorf("gateway API credential is not configured")
		}
		client, err := gatewayclient.New(endpoint, apiKey, nil)
		if err != nil {
			return nil, "", err
		}
		return client, client.Endpoint(), nil
	}
	path, err := absoluteEnrolledManifest(manifestPath)
	if err != nil {
		return nil, "", err
	}
	client, err := gatewayclient.NewEnrolledFromManifest(endpoint, path)
	if err != nil {
		return nil, "", err
	}
	return client, client.Endpoint(), nil
}

func absoluteEnrolledManifest(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
