package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
)

func TestCloseFleetConversationAuthorization(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	now := time.Now().UTC()
	if _, err := store.ReportPresenceWithPrincipal(ctx, storage.PresenceReport{
		Source: "fleet-test", Sequence: 1, Agents: []storage.PresenceAgent{{ProjectIdent: "tasks", Origin: *actor}},
	}, now, "owner-node"); err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "fixture")
	mux := http.NewServeMux()
	a.registerFleetConversationRoutes(mux)
	for _, test := range []struct {
		name, principal, role, project string
		status                         int
	}{
		{"bearer only", "", "", "tasks", http.StatusForbidden},
		{"foreign node", "other-node", "client", "tasks", http.StatusForbidden},
		{"wrong project", "owner-node", "client", "other", http.StatusNotFound},
		{"owner", "owner-node", "client", "tasks", http.StatusNoContent},
		{"administrator", "admin-node", "administrator", "tasks", http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat, err := store.OpenFleetConversation(ctx, "tasks", actor.SessionID, now)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodDelete, "/v1/projects/"+test.project+"/fleet-conversations/"+chat.ID, nil)
			if test.principal != "" {
				r = r.WithContext(context.WithValue(r.Context(), securePrincipalContextKey{}, &securePrincipal{NodeID: test.principal, role: test.role}))
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status %d, want %d: %s", w.Code, test.status, w.Body)
			}
			chats, err := store.FleetConversations(ctx, "owner-node")
			if err != nil {
				t.Fatal(err)
			}
			if closed := len(chats) == 0; closed != (test.status == http.StatusNoContent) {
				t.Fatalf("closed=%v after status %d", closed, w.Code)
			}
		})
	}
}
