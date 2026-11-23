package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/storage"
)

func TestConfiguredDiscordProjectMessagesUseProviderRoomsAndReceipts(t *testing.T) {
	var mu sync.Mutex
	var providerCalls []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		providerCalls = append(providerCalls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bot synthetic-token" {
			t.Error("provider request lacks synthetic bot authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /guilds/100/channels":
			fmt.Fprint(w, `[]`)
		case "POST /guilds/100/channels":
			var input struct {
				Name string `json:"name"`
				Type int    `json:"type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Name != "external" || input.Type != 0 {
				t.Errorf("invalid room creation: %+v %v", input, err)
			}
			fmt.Fprint(w, `{"id":"111","name":"external","type":0}`)
		case "POST /channels/111/messages":
			var input struct {
				Content   string            `json:"content"`
				Embeds    []json.RawMessage `json:"embeds"`
				Reference *struct {
					MessageID string `json:"message_id"`
				} `json:"message_reference"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			switch len(providerCalls) {
			case 3:
				if len(input.Embeds) != 1 || input.Reference != nil || input.Content != "" {
					t.Errorf("root did not use structured provider send: %+v", input)
				}
				fmt.Fprint(w, `{"id":"222","channel_id":"111"}`)
			case 4:
				if len(input.Embeds) != 1 || input.Reference == nil || input.Reference.MessageID != "222" {
					t.Errorf("reply lost provider parent: %+v", input)
				}
				fmt.Fprint(w, `{"id":"333","channel_id":"111"}`)
			case 5:
				if len(input.Embeds) != 0 || input.Content != "**Reviewer** (via gateway): human reply" || input.Reference == nil || input.Reference.MessageID != "222" {
					t.Errorf("human post lost raw body or normalized provider parent: %+v", input)
				}
				fmt.Fprint(w, `{"id":"444","channel_id":"111"}`)
			default:
				t.Errorf("unexpected provider send count: %d", len(providerCalls))
				w.WriteHeader(http.StatusInternalServerError)
			}
		default:
			t.Errorf("unexpected provider operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()
	discord, err := channels.NewDiscord(channels.DiscordConfig{Token: "synthetic-token", GuildID: "100", APIBaseURL: provider.URL, HTTPClient: provider.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "discord.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewHandlerWithConfig(store, "task-key", HandlerConfig{Channels: map[string]channels.Outbound{"discord": discord}})
	if err != nil {
		t.Fatal(err)
	}
	registered := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"external","channel":"discord","repo_url":"https://github.com/example/external.git"}`, nil)
	if registered.Code != http.StatusOK {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	var project projectRegistration
	if err := json.Unmarshal(registered.Body.Bytes(), &project); err != nil || project.Channel != "discord" || project.Room != "111" || project.Kind != "repo" {
		t.Fatalf("provider room not persisted: %+v %v", project, err)
	}
	send := func(path, body string) agentProjectMessageReceipt {
		t.Helper()
		response := taskRequest(handler, http.MethodPost, path, body, apiActor())
		var receipt agentProjectMessageReceipt
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &receipt) != nil {
			t.Fatalf("send: %d %s", response.Code, response.Body.String())
		}
		return receipt
	}
	root := send("/v1/projects/external/messages", `{"body":"root"}`)
	reply := send(fmt.Sprintf("/v1/projects/external/messages/%d/reply", root.MessageID), `{"body":"reply"}`)
	if root.ExternalMessageID != "222" || reply.ExternalMessageID != "333" || reply.ParentMessageID == nil || *reply.ParentMessageID != root.MessageID {
		t.Fatalf("provider receipts not retained: root=%+v reply=%+v", root, reply)
	}
	human := taskRequest(handler, http.MethodPost, "/v1/projects/external/messages/human", fmt.Sprintf(`{"content":" human reply ","author":" Reviewer ","parent_message_id":%d}`, reply.MessageID), nil)
	var humanReceipt humanProjectMessageResponse
	if human.Code != http.StatusOK || json.Unmarshal(human.Body.Bytes(), &humanReceipt) != nil || !humanReceipt.Delivered || humanReceipt.ParentMessageID == nil || *humanReceipt.ParentMessageID != root.MessageID {
		t.Fatalf("human delivery: %d %s", human.Code, human.Body.String())
	}
	for id, external := range map[int64]string{root.MessageID: "222", reply.MessageID: "333", humanReceipt.MessageID: "444"} {
		message, err := store.ProjectMessage(ctx, "external", id)
		if err != nil || message == nil || message.ExternalMessageID == nil || *message.ExternalMessageID != external {
			t.Fatalf("stored provider receipt %d: %+v %v", id, message, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(providerCalls) != 5 {
		t.Fatalf("unexpected provider calls: %+v", providerCalls)
	}
}
