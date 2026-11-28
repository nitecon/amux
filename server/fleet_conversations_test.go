package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
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

func TestFailoverFleetConversationAuthorization(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	now := time.Now().UTC()
	if _, err := store.ReportPresenceWithPrincipal(ctx, storage.PresenceReport{
		Source: "fleet-test", Sequence: 1, Agents: []storage.PresenceAgent{{ProjectIdent: "tasks", Origin: *actor}},
	}, now, "owner-node"); err != nil {
		t.Fatal(err)
	}
	chat, err := store.OpenFleetConversation(ctx, "tasks", actor.SessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "fixture")
	mux := http.NewServeMux()
	a.registerFleetConversationRoutes(mux)
	send := func(principal string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/projects/tasks/fleet-conversations/"+chat.ID+"/failover", nil)
		if principal != "" {
			r = r.WithContext(context.WithValue(r.Context(), securePrincipalContextKey{}, &securePrincipal{NodeID: principal, role: "client"}))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if code := send(""); code != http.StatusForbidden {
		t.Fatalf("bearer only status %d", code)
	}
	if code := send("other-node"); code != http.StatusForbidden {
		t.Fatalf("foreign node status %d", code)
	}
	if code := send("owner-node"); code != http.StatusConflict {
		t.Fatalf("no candidate status %d", code)
	}
	spare := *actor
	spare.SessionID, spare.InstanceID, spare.Provider = uuid.NewString(), uuid.NewString(), "claude"
	if _, err := store.ReportPresenceWithPrincipal(ctx, storage.PresenceReport{
		Source: "fleet-pool/spare", Sequence: 1, Agents: []storage.PresenceAgent{{ProjectIdent: "tasks", Origin: spare}},
	}, now, "spare-node"); err != nil {
		t.Fatal(err)
	}
	if code := send("owner-node"); code != http.StatusOK {
		t.Fatalf("owner failover status %d", code)
	}
	if chats, err := store.FleetConversations(ctx, "spare-node"); err != nil || len(chats) != 1 || chats[0].ID != chat.ID {
		t.Fatalf("spare node listed %+v, %v", chats, err)
	}
}

func TestFailoverFleetConversationRejectsIncompleteExpectation(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	now := time.Now().UTC()
	spare := *actor
	spare.SessionID, spare.InstanceID = uuid.NewString(), uuid.NewString()
	for source, origin := range map[string]storage.PresenceAgent{
		"fleet-pool/owner": {ProjectIdent: "tasks", Origin: *actor},
		"fleet-pool/spare": {ProjectIdent: "tasks", Origin: spare},
	} {
		if _, err := store.ReportPresenceWithPrincipal(ctx, storage.PresenceReport{Source: source, Sequence: 1, Agents: []storage.PresenceAgent{origin}}, now, "owner-node"); err != nil {
			t.Fatal(err)
		}
	}
	chat, err := store.OpenFleetConversation(ctx, "tasks", actor.SessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindConversationSession(ctx, "tasks", chat.ID, "thread", actor); err != nil {
		t.Fatal(err)
	}
	prompt, err := store.EnqueueConversationCommand(ctx, "tasks", chat.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "fixture")
	mux := http.NewServeMux()
	a.registerFleetConversationRoutes(mux)
	send := func(body string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/projects/tasks/fleet-conversations/"+chat.ID+"/failover", strings.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), securePrincipalContextKey{}, &securePrincipal{NodeID: "owner-node", role: "client"}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	for _, body := range []string{
		`{`,
		`null`,
		`{}`,
		`[]`,
		`{"instance_id":"stale"}`,
		`{"session_id":"` + actor.SessionID + `"}`,
		`{"session_id":"` + actor.SessionID + `","instance_id":"` + actor.InstanceID + `"}`,
		`{"instance_id":"` + actor.InstanceID + `","native_session_id":"thread"}`,
		`{"session_id":"` + actor.SessionID + `","instance_id":"` + actor.InstanceID + `","native_session_id":"thread","extra":1}`,
	} {
		if code := send(body); code != http.StatusBadRequest {
			t.Fatalf("body %s status %d", body, code)
		}
		current, err := store.Conversation(ctx, "tasks", chat.ID)
		if err != nil || current.Owner != *actor || current.NativeSessionID != "thread" {
			t.Fatalf("body %s changed chat %+v, %v", body, current, err)
		}
		commands, err := store.ConversationCommands(ctx, "tasks", chat.ID, 0, 10)
		if err != nil || len(commands) != 1 || commands[0].ID != prompt.ID || commands[0].State != "queued" || commands[0].TargetInstanceID != actor.InstanceID {
			t.Fatalf("body %s changed commands %+v, %v", body, commands, err)
		}
	}
	// The worker's complete expectation still moves the chat.
	complete, err := json.Marshal(storage.FleetFailoverExpectation{SessionID: actor.SessionID, InstanceID: actor.InstanceID, NativeSessionID: "thread"})
	if err != nil {
		t.Fatal(err)
	}
	if code := send(string(complete)); code != http.StatusOK {
		t.Fatalf("complete expectation status %d", code)
	}
	if current, err := store.Conversation(ctx, "tasks", chat.ID); err != nil || current.Owner != spare || current.NativeSessionID != "" {
		t.Fatalf("complete expectation left chat %+v, %v", current, err)
	}
}
