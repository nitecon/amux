package worksubmission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxPreferencesBytes = 1 << 20

// PreferencesDocument is the nested work_submission object shown by the CLI.
type PreferencesDocument struct {
	WorkSubmission *Preferences `json:"work_submission"`
}

// PreferencesPath resolves the shared CLI, desktop, and worker preferences
// file. An explicit path takes precedence over the configured environment path.
func PreferencesPath(override, configuredEnv string) (string, error) {
	path := strings.TrimSpace(override)
	if path == "" {
		path = strings.TrimSpace(configuredEnv)
	}
	if path == "" {
		directory, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve user configuration directory: %w", err)
		}
		path = filepath.Join(directory, "amux", "work-preferences.json")
	}
	return filepath.Abs(path)
}

// OptionalPreferencesPath uses the shared default path when available, but
// permits in-memory defaults when no path was configured and OS lookup fails.
func OptionalPreferencesPath(override, configuredEnv string) (string, error) {
	path, err := PreferencesPath(override, configuredEnv)
	if err != nil && strings.TrimSpace(override) == "" && strings.TrimSpace(configuredEnv) == "" {
		return "", nil
	}
	return path, err
}

// LoadPreferences reads work_submission from the shared preferences document.
// A missing file or omitted nested fields use the source-compatible defaults.
func LoadPreferences(path string) (Preferences, error) {
	preferences := DefaultPreferences()
	if path == "" {
		return preferences, errors.New("work preferences path is required")
	}
	root, err := readPreferencesRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return preferences, nil
	}
	if err != nil {
		return preferences, fmt.Errorf("read work submission preferences: %w", err)
	}
	raw, exists := root["work_submission"]
	if !exists {
		return preferences, nil
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return preferences, errors.New("work_submission preferences cannot be null")
	}
	fields, ok := rawPreferencesObject(raw)
	if !ok {
		return preferences, errors.New("work submission preferences must be an object")
	}
	if value, found := fields["mode"]; found && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return preferences, errors.New("work submission mode cannot be null")
	}
	if value, found := fields["branch_prefix"]; found && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return preferences, errors.New("work submission branch prefix cannot be null")
	}
	var stored struct {
		Mode         *Mode   `json:"mode"`
		BranchPrefix *string `json:"branch_prefix"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return preferences, fmt.Errorf("work submission preferences are invalid: %w", err)
	}
	if stored.Mode != nil {
		preferences.Mode = *stored.Mode
	}
	if stored.BranchPrefix != nil {
		preferences.BranchPrefix = *stored.BranchPrefix
	}
	if err := validateMode(preferences.Mode); err != nil {
		return preferences, err
	}
	return preferences, nil
}

// SavePreferences atomically replaces only work_submission, preserving any
// other root fields and refusing to overwrite an invalid existing document.
func SavePreferences(path string, preferences Preferences) error {
	if path == "" {
		return errors.New("work preferences path is required")
	}
	if err := validateMode(preferences.Mode); err != nil {
		return err
	}
	unlock, err := lockPreferences(path)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := LoadPreferences(path); err != nil {
		return fmt.Errorf("refusing to overwrite invalid work preferences: %w", err)
	}
	encoded, err := json.Marshal(preferences)
	if err != nil {
		return err
	}
	return writePreferencesKey(path, "work_submission", encoded)
}

// lockPreferences takes an exclusive lock on a sibling lock file so CLI,
// desktop and worker processes serialize read-modify-write updates of the
// shared document. The rename-replaced document itself cannot carry the lock.
func lockPreferences(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create work preference directory: %w", err)
	}
	file, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open work preference lock: %w", err)
	}
	if err := lockPreferencesFile(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock work preferences: %w", err)
	}
	return func() { file.Close() }, nil
}

// writePreferencesFile atomically replaces the shared preferences document.
func writePreferencesFile(path string, data []byte) error {
	if len(data) > maxPreferencesBytes {
		return errors.New("work preferences exceed 1 MiB")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create work preference directory: %w", err)
	}
	file, err := os.CreateTemp(parent, ".work-preferences-*")
	if err != nil {
		return fmt.Errorf("create work preference file: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("save work submission preferences: %w", err)
	}
	return nil
}

func readPreferencesRoot(path string) (map[string]json.RawMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPreferencesBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPreferencesBytes {
		return nil, errors.New("work preferences exceed 1 MiB")
	}
	root, ok := rawPreferencesObject(data)
	if !ok {
		return nil, errors.New("work preferences must contain a JSON object")
	}
	return root, nil
}

func rawPreferencesObject(data []byte) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}
