package server

import (
	"context"
	"errors"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/storage"
	"github.com/rs/zerolog/log"
)

func maintainSecureCredentials(ctx context.Context, bus *eventbus.Bus, connections *SecureConnections, enrollment *storage.SecurityEnrollment, localURL, instanceID, profile, clusterID string, names []string) {
	reconcile := func(now time.Time) {
		_, provisionErr := enrollment.ProvisionLocal(ctx, localURL, instanceID)
		retireErr := connections.RetireCredentials(ctx, enrollment, now)
		rotationErr := connections.RefreshServerCertificate(enrollment, profile, clusterID, names, now)
		if err := errors.Join(provisionErr, retireErr, rotationErr); err != nil && ctx.Err() == nil {
			log.Warn().Ctx(ctx).Err(err).Msg("secure credential maintenance failed")
		}
	}
	for ctx.Err() == nil {
		subscription := bus.Subscribe(64)
		reconcile(time.Now())
		err := followCredentialTicks(ctx, subscription, reconcile)
		subscription.Unsubscribe()
		if !errors.Is(err, eventbus.ErrSlowConsumer) {
			return
		}
	}
}

func followCredentialTicks(ctx context.Context, subscription *eventbus.Subscription, reconcile func(time.Time)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-subscription.C:
			if !ok {
				return subscription.Err()
			}
			if event.Topic == eventbus.Tick {
				reconcile(event.At)
			}
		}
	}
}
