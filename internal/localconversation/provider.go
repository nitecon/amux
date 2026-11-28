package localconversation

import (
	"fmt"

	"github.com/nitecon/amux/providers"
)

// LaunchCommand builds a command using the selected provider's preferences.
func LaunchCommand(id string, prefs providers.Preferences) ([]string, error) {
	provider, ok := providers.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("unsupported provider %q", id)
	}
	return provider.LaunchCommand(prefs)
}

func ProviderLabel(id string) string {
	if provider, ok := providers.Lookup(id); ok {
		return provider.Label()
	}
	return id
}
