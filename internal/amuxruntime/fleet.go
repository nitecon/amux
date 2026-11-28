package amuxruntime

import (
	"context"
	"fmt"
	"os"

	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/gatewayclient"
)

// CloseFleetConversation uses the configured enrolled client without starting
// the companion or its hosted gateway.
func CloseFleetConversation(ctx context.Context, project, id string) error {
	settings := config.Get()
	client, _, err := gatewayclient.NewForProfile(settings.GatewayEndpoint, settings.EnrolledProfile, os.Getenv(settings.APIKeyEnv))
	if err != nil {
		return err
	}
	if err := client.CloseFleetConversation(ctx, project, id); err != nil {
		return fmt.Errorf("close fleet conversation: %w", err)
	}
	return nil
}
