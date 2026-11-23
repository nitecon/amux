package server

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/nitecon/amux/eventbus"
)

// Navigation notifications invalidate read models. They carry no business
// payload or cursor; every connection starts with an authoritative reload.
func (a *api) navigationStream(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		writeError(w, 400, "navigation stream accepts no query parameters")
		return
	}
	a.withStream(w, r, 4096, func(ctx context.Context, c *websocket.Conn) {
		sub := a.store.EventBus().Subscribe(32)
		defer sub.Unsubscribe()
		closed := c.CloseRead(ctx)
		if sendFrame(ctx, c, map[string]any{"version": 1, "type": "ready"}) != nil {
			return
		}
		for {
			select {
			case <-closed.Done():
				return
			case event, ok := <-sub.C:
				if !ok {
					return
				} // Overflow forces reconnect and a full reload.
				switch event.Topic {
				case eventbus.ProjectChanged, eventbus.TaskChanged, eventbus.ConversationChanged, eventbus.PresenceChanged, eventbus.Tick:
					if sendFrame(ctx, c, map[string]any{"version": 1, "type": "changed"}) != nil {
						return
					}
				}
			}
		}
	})
}
