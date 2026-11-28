package config

import (
	"context"
	"fmt"

	"github.com/nitecon/amux/storage"
)

type fleetSource interface {
	ProviderSettings(context.Context) ([]storage.ProviderSetting, error)
}

// fleetProviders reads the gateway's provider switches and applies this device's optional provider selection.
func fleetProviders(ctx context.Context, gateway fleetSource, local []string) ([]string, error) {
	settings, err := gateway.ProviderSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("load fleet provider settings: %w", err)
	}
	enabled := make([]string, 0, len(settings))
	for _, provider := range settings {
		if !provider.Enabled {
			continue
		}
		if local == nil {
			enabled = append(enabled, provider.ID)
		} else {
			for _, allowed := range local {
				if allowed == provider.ID {
					enabled = append(enabled, provider.ID)
					break
				}
			}
		}
	}
	return enabled, nil
}
