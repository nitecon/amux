package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestGatewayClientForEnrolledManifestUsesPinnedIdentityAndEndpoint(t *testing.T) {
	profile := t.TempDir()
	address, stop := startServerCommand(t, "--profile", profile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", uuid.NewString(), "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stop()
	manifest := filepath.Join(profile, "local-client.json")
	client, endpoint, err := gatewayClientForProfile("https://"+address, manifest, "ignored-bearer")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://"+address || client.Endpoint() != endpoint {
		t.Fatalf("resolved enrolled endpoint = %q, client=%q", endpoint, client.Endpoint())
	}
	if _, _, err := gatewayClientForProfile("https://other.example", manifest, "ignored-bearer"); err == nil {
		t.Fatal("enrolled profile accepted a different explicit endpoint")
	}
	if _, err := client.Projects(context.Background()); err != nil {
		t.Fatalf("enrolled profile request failed: %v", err)
	}
}

func TestGatewayClientForProfilePreservesBearerAndRequiresManifestPath(t *testing.T) {
	client, endpoint, err := gatewayClientForProfile("http://127.0.0.1:7913", "", "bearer-secret")
	if err != nil || endpoint != "http://127.0.0.1:7913" || client == nil {
		t.Fatalf("bearer selection = %v, %q, %v", client, endpoint, err)
	}
	if _, _, err := gatewayClientForProfile("", "", ""); err == nil {
		t.Fatal("missing gateway credentials were accepted")
	}
	if _, _, err := gatewayClientForProfile("https://gateway.example", "/profile", "bearer-secret"); err == nil {
		t.Fatal("profile directory accepted where manifest path is required")
	}
}

func TestEnrolledMCPKeepsNativeTaskToolsAndCallsGateway(t *testing.T) {
	profile := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := store.InsertTask(context.Background(), storage.TaskInsert{Project: "demo", Title: "Enrolled tool task", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	address, stop := startServerCommand(t, "--profile", profile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", uuid.NewString(), "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stop()
	manifest := filepath.Join(profile, "local-client.json")
	client, _, err := gatewayClientForProfile("https://"+address, manifest, "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID, instanceID := uuid.NewString(), uuid.NewString()
	owner := storage.AgentOrigin{SessionID: sessionID, InstanceID: instanceID, Provider: "codex", OS: "linux"}
	conversation, err := client.CreateConversation(context.Background(), "demo", task.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"task_list","arguments":{}}}`,
		"",
	}
	var stdout bytes.Buffer
	err = runMCP(context.Background(), []string{"--server", "https://" + address, "--enrolled-profile", manifest,
		"--project", "demo", "--conversation", conversation.ID, "--session", sessionID,
		"--instance", instanceID, "--provider", "codex", "--os", "linux"},
		io.NopCloser(strings.NewReader(strings.Join(lines, "\n"))), &stdout, io.Discard, func(string) string { return "unused-bearer" })
	if err != nil {
		t.Fatalf("enrolled MCP failed: %v; output=%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"name":"task_list"`) {
		t.Fatalf("enrolled MCP omitted native task tools: %s", stdout.String())
	}
	var taskCall struct {
		ID     int `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal([]byte(line), &response) == nil && response.ID == 3 {
			taskCall.ID = response.ID
			if err := json.Unmarshal(response.Result, &taskCall.Result); err != nil {
				t.Fatal(err)
			}
		}
	}
	if taskCall.ID != 3 || taskCall.Result.IsError {
		t.Fatalf("enrolled task_list invocation failed: %s", stdout.String())
	}
}
