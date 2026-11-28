package gatewayclient

import (
	"fmt"
	"path/filepath"
)

// NewForProfile selects one authentication mode. An enrolled
// profile option names a credential manifest directly; bearer credentials are
// never combined with it.
func NewForProfile(endpoint, manifestPath, apiKey string) (*Client, string, error) {
	if manifestPath == "" {
		if endpoint == "" {
			return nil, "", fmt.Errorf("gateway server is required")
		}
		if apiKey == "" {
			return nil, "", fmt.Errorf("gateway API credential is not configured")
		}
		client, err := New(endpoint, apiKey, nil)
		if err != nil {
			return nil, "", err
		}
		return client, client.Endpoint(), nil
	}
	path, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, "", err
	}
	client, err := NewEnrolledFromManifest(endpoint, path)
	if err != nil {
		return nil, "", err
	}
	return client, client.Endpoint(), nil
}
