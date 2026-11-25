package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/storage"
)

func (a *api) registerFleetConversationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/fleet-agents/{session}/conversation", a.openFleetConversation)
	mux.HandleFunc("GET /v1/fleet/conversations/stream", a.fleetConversationStream)
}

func (a *api) openFleetConversation(w http.ResponseWriter, r *http.Request) {
	if authenticatedSecurePrincipal(r.Context()) == nil {
		writeError(w, http.StatusForbidden, "fleet chat requires an enrolled client")
		return
	}
	info, err := a.store.OpenFleetConversation(r.Context(), r.PathValue("ident"), r.PathValue("session"), time.Now().UTC())
	if errors.Is(err, storage.ErrFleetAgentUnavailable) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, info)
}

func (a *api) fleetConversationStream(w http.ResponseWriter, r *http.Request) {
	peer := authenticatedSecurePrincipal(r.Context())
	if peer == nil {
		writeError(w, http.StatusForbidden, "fleet chat requires an enrolled client")
		return
	}
	a.withStream(w, r, 4096, func(ctx context.Context, connection *websocket.Conn) {
		sub := a.store.EventBus().Subscribe(32)
		defer sub.Unsubscribe()
		closed := connection.CloseRead(ctx)
		send := func() error {
			items, err := a.store.FleetConversations(ctx, peer.NodeID)
			if err != nil {
				return err
			}
			return sendFrame(ctx, connection, struct {
				Version int                         `json:"version"`
				Chats   []storage.FleetConversation `json:"chats"`
			}{1, items})
		}
		if err := send(); err != nil {
			return
		}
		for {
			select {
			case <-closed.Done():
				return
			case event, ok := <-sub.C:
				if !ok {
					return
				}
				if event.Topic == eventbus.ProjectChanged || event.Topic == eventbus.FleetConversationChanged {
					if err := send(); err != nil {
						return
					}
				}
			}
		}
	})
}
