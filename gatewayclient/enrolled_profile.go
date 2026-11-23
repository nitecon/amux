package gatewayclient

import (
	"fmt"
	"path/filepath"
)

// NewEnrolledFromManifest loads a validated profile-backed identity and
// requires any explicit endpoint to match the manifest exactly. Its shared
// HTTP and WebSocket transport checks for peer renewal and credential reload
// before each connection.
func NewEnrolledFromManifest(endpoint, manifestPath string) (*Client, error) {
	path, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, err
	}
	switch filepath.Base(path) {
	case "client-profile.json", "local-client.json":
	default:
		return nil, fmt.Errorf("enrolled profile must name a client-profile.json or local-client.json manifest")
	}
	return newManifestClient(endpoint, path)
}
