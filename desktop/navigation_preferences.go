package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxNavigationPreferencesSize = 1 << 20

type NavigationPreferences struct {
	Favorites           map[string]bool                          `json:"favorites"`
	Selections          map[string]NavigationSelection           `json:"selections"`
	WorkerPools         map[string]string                        `json:"worker_pools,omitempty"`
	ProjectsDirectories map[string]string                        `json:"projects_directories,omitempty"`
	WorkspaceIDs        map[string][]string                      `json:"workspace_ids,omitempty"`
	ActiveWorkspace     map[string]string                        `json:"active_workspace,omitempty"`
	ConversationLaunch  map[string]ConversationLaunchPreferences `json:"conversation_launch,omitempty"`
}

type ConversationLaunchPreferences struct {
	Model   string `json:"model,omitempty"`
	Persona string `json:"persona,omitempty"`
}

func workspaceSelectionKey(endpoint, workspaceID string) string {
	return endpoint + "\x00workspace\x00" + workspaceID
}

type NavigationSelection struct {
	Project        string  `json:"project"`
	WorkspaceName  string  `json:"workspace_name,omitempty"`
	CheckoutPath   string  `json:"checkout_path,omitempty"`
	CheckoutOrigin *string `json:"checkout_origin,omitempty"`
	Task           string  `json:"task"`
	Conversation   string  `json:"conversation"`
	ProjectFilter  string  `json:"project_filter"`
	TaskFilter     string  `json:"task_filter"`
	Status         string  `json:"status"`
	MainView       string  `json:"main_view,omitempty"`
}

type navigationPreferencesFile struct {
	Version     int                    `json:"version"`
	Preferences *NavigationPreferences `json:"preferences"`
}

func emptyNavigationPreferences() NavigationPreferences {
	return NavigationPreferences{
		Favorites:  make(map[string]bool),
		Selections: make(map[string]NavigationSelection),
	}
}

func normalizedNavigationPreferences(prefs NavigationPreferences) NavigationPreferences {
	if prefs.Favorites == nil {
		prefs.Favorites = make(map[string]bool)
	}
	if prefs.Selections == nil {
		prefs.Selections = make(map[string]NavigationSelection)
	}
	return prefs
}

// LoadNavigationPreferences reads only the explicitly supplied preferences file.
// A missing file is an empty in-memory preference set; corrupt data is returned
// as an error so callers cannot silently replace it with defaults.
func LoadNavigationPreferences(path string) (NavigationPreferences, error) {
	if path == "" {
		return NavigationPreferences{}, errors.New("navigation preferences path is required")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyNavigationPreferences(), nil
	}
	if err != nil {
		return NavigationPreferences{}, fmt.Errorf("inspect navigation preferences: %w", err)
	}
	if !info.Mode().IsRegular() {
		return NavigationPreferences{}, errors.New("navigation preferences path must be a regular file")
	}
	if info.Size() > maxNavigationPreferencesSize {
		return NavigationPreferences{}, errors.New("navigation preferences file exceeds 1 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return NavigationPreferences{}, fmt.Errorf("open navigation preferences: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxNavigationPreferencesSize+1))
	if err != nil {
		return NavigationPreferences{}, fmt.Errorf("read navigation preferences: %w", err)
	}
	if len(data) > maxNavigationPreferencesSize {
		return NavigationPreferences{}, errors.New("navigation preferences file exceeds 1 MiB")
	}
	var saved navigationPreferencesFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return NavigationPreferences{}, fmt.Errorf("decode navigation preferences: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return NavigationPreferences{}, fmt.Errorf("decode navigation preferences: %w", err)
	}
	if saved.Version != 1 {
		return NavigationPreferences{}, fmt.Errorf("unsupported navigation preferences version %d", saved.Version)
	}
	if saved.Preferences == nil {
		return NavigationPreferences{}, errors.New("navigation preferences are missing")
	}
	return normalizedNavigationPreferences(*saved.Preferences), nil
}

// SaveNavigationPreferences atomically writes only the explicitly supplied path.
func SaveNavigationPreferences(path string, prefs NavigationPreferences) error {
	if path == "" {
		return errors.New("navigation preferences path is required")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("navigation preferences path must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect navigation preferences: %w", err)
	}
	normalized := normalizedNavigationPreferences(prefs)
	data, err := json.MarshalIndent(navigationPreferencesFile{Version: 1, Preferences: &normalized}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode navigation preferences: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxNavigationPreferencesSize {
		return errors.New("navigation preferences file exceeds 1 MiB")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create navigation preferences directory: %w", err)
	}
	tmp, err := os.CreateTemp(parent, ".navigation-preferences-*.tmp")
	if err != nil {
		return fmt.Errorf("create navigation preferences temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set navigation preferences permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write navigation preferences: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync navigation preferences: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close navigation preferences: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace navigation preferences: %w", err)
	}
	return nil
}
