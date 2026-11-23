package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type GatewayMode string

const (
	GatewayModeServer GatewayMode = "server"
	GatewayModeClient GatewayMode = "client"
)

// GatewayPreferences stores only connection selection and the enrolled
// manifest path. Peering invitations are short lived and are never persisted.
type GatewayPreferences struct {
	Mode             GatewayMode `json:"mode"`
	Endpoint         string      `json:"endpoint,omitempty"`
	EnrolledProfile  string      `json:"enrolled_profile,omitempty"`
	ServerInstanceID string      `json:"server_instance_id,omitempty"`
	RemoteHosting    bool        `json:"remote_hosting,omitempty"`
	AdvertisedHost   string      `json:"advertised_host,omitempty"`
}

func NormalizeAdvertisedHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("an advertised hostname or IP address is required")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if len(host) > 253 || strings.ContainsAny(host, ":/@?#[] \t\r\n") {
		return "", errors.New("advertised host must be a hostname or IP address without a port")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "", errors.New("advertised host is required")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("advertised hostname has an invalid DNS label")
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return "", errors.New("advertised host must be a hostname or IP address")
			}
		}
	}
	return host, nil
}

type gatewayPreferencesFile struct {
	Version     int                `json:"version"`
	Preferences GatewayPreferences `json:"preferences"`
}

func DefaultGatewayPreferences() GatewayPreferences {
	return GatewayPreferences{Mode: GatewayModeServer}
}

func LoadGatewayPreferences(path string) (GatewayPreferences, error) {
	if path == "" {
		return GatewayPreferences{}, errors.New("gateway preferences path is required")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultGatewayPreferences(), nil
	}
	if err != nil {
		return GatewayPreferences{}, fmt.Errorf("inspect gateway preferences: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return GatewayPreferences{}, errors.New("gateway preferences must be a regular file smaller than 1 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return GatewayPreferences{}, fmt.Errorf("read gateway preferences: %w", err)
	}
	var saved gatewayPreferencesFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return GatewayPreferences{}, fmt.Errorf("decode gateway preferences: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return GatewayPreferences{}, errors.New("decode gateway preferences: trailing data")
	}
	if saved.Version != 1 || (saved.Preferences.Mode != GatewayModeServer && saved.Preferences.Mode != GatewayModeClient) {
		return GatewayPreferences{}, errors.New("unsupported gateway preferences")
	}
	if saved.Preferences.Mode == GatewayModeClient && (saved.Preferences.Endpoint == "" || saved.Preferences.EnrolledProfile == "") {
		return GatewayPreferences{}, errors.New("client gateway preferences require an endpoint and enrolled profile")
	}
	if saved.Preferences.RemoteHosting {
		if saved.Preferences.Mode != GatewayModeServer {
			return GatewayPreferences{}, errors.New("remote hosting is available only in server mode")
		}
		if _, err := NormalizeAdvertisedHost(saved.Preferences.AdvertisedHost); err != nil {
			return GatewayPreferences{}, err
		}
	}
	return saved.Preferences, nil
}

func SaveGatewayPreferences(path string, prefs GatewayPreferences) error {
	if path == "" {
		return errors.New("gateway preferences path is required")
	}
	if prefs.Mode != GatewayModeServer && prefs.Mode != GatewayModeClient {
		return errors.New("invalid gateway mode")
	}
	if prefs.Mode == GatewayModeClient && (prefs.Endpoint == "" || prefs.EnrolledProfile == "") {
		return errors.New("client gateway preferences require an endpoint and enrolled profile")
	}
	if prefs.RemoteHosting {
		if prefs.Mode != GatewayModeServer {
			return errors.New("remote hosting is available only in server mode")
		}
		if _, err := NormalizeAdvertisedHost(prefs.AdvertisedHost); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(gatewayPreferencesFile{Version: 1, Preferences: prefs}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create gateway preferences directory: %w", err)
	}
	tmp, err := os.CreateTemp(parent, ".gateway-preferences-*.tmp")
	if err != nil {
		return fmt.Errorf("create gateway preferences temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("save gateway preferences: %w", err)
	}
	return nil
}
