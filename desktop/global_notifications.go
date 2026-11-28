package desktop

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/rs/zerolog/log"
)

func (b *Browser) configureGlobalNotifications(notify func(string)) {
	if notify == nil {
		return
	}
	scope := b.client.NotificationScope()
	id := b.prefs.NotificationConsumers[scope]
	if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
		id = uuid.NewString()
		if b.prefs.NotificationConsumers == nil {
			b.prefs.NotificationConsumers = make(map[string]string)
		}
		b.prefs.NotificationConsumers[scope] = id
		if b.prefsPath != "" {
			if err := SaveNavigationPreferences(b.prefsPath, b.prefs); err != nil {
				log.Warn().Err(err).Msg("save desktop notification subscription")
				return
			}
		}
	}
	b.notificationConsumer = id
	b.notifyGlobal = notify
}

func (b *Browser) watchGlobalNotifications(ctx context.Context) {
	const minBackoff = 250 * time.Millisecond
	backoff := minBackoff
	var lastUID string
	for ctx.Err() == nil {
		err := b.client.WatchGlobalNotifications(ctx, b.notificationConsumer, func(uid, message string) error {
			backoff = minBackoff
			if uid == lastUID {
				return nil
			}
			displayed := make(chan struct{})
			b.dispatch(func() {
				if ctx.Err() == nil {
					b.notifyGlobal(message)
					close(displayed)
				}
			})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-displayed:
				lastUID = uid
				return nil
			}
		})
		var response *gatewayclient.HTTPError
		if errors.As(err, &response) && response.Status == http.StatusNotFound {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}
