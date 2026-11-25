package desktop

import (
	"encoding/base64"
	"path/filepath"
	"strings"
)

// ClientCheckoutSettings extracts the saved checkout settings scoped to the
// selected gateway. Navigation choices stay in the desktop preferences.
func (prefs NavigationPreferences) ClientCheckoutSettings(endpoint string) (string, map[string]string) {
	directory := prefs.ProjectsDirectory
	if directory == "" {
		directory = prefs.ProjectsDirectories[endpoint]
	}
	prefix := "remote:" + base64.RawURLEncoding.EncodeToString([]byte(endpoint)) + ":"
	bindings := make(map[string]string)
	for key, binding := range prefs.ProjectCheckouts {
		suffix, matches := strings.CutPrefix(key, prefix)
		if !matches || !filepath.IsAbs(binding.Directory) {
			continue
		}
		id, err := base64.RawURLEncoding.DecodeString(suffix)
		if err == nil && len(id) > 0 {
			bindings[string(id)] = binding.Directory
		}
	}
	return directory, bindings
}
