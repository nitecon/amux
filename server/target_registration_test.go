package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
)

func TestTargetRegistrationRoutes(t *testing.T) {
	store, handler := taskAPI(t)
	actor := apiActor()
	path := "/v1/projects/tasks/task-targets"
	body := `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","display_name":" Worker ","hostname":"host"}`
	registered := taskRequest(handler, http.MethodPost, path, body, actor)
	if registered.Code != http.StatusOK {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	var responseShape map[string]json.RawMessage
	if err := json.Unmarshal(registered.Body.Bytes(), &responseShape); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"target", "seen_at", "expires_at", "canonical_remote", "display_name", "hostname"} {
		if _, ok := responseShape[field]; !ok {
			t.Fatalf("POST response omitted %q: %s", field, registered.Body.String())
		}
	}
	if string(responseShape["canonical_remote"]) != "null" {
		t.Fatalf("POST canonical_remote=%s, want null for ad-hoc target", responseShape["canonical_remote"])
	}
	var displayName, hostname string
	if err := json.Unmarshal(responseShape["display_name"], &displayName); err != nil || displayName != "Worker" {
		t.Fatalf("normalized display_name=%q, err=%v", displayName, err)
	}
	if err := json.Unmarshal(responseShape["hostname"], &hostname); err != nil || hostname != "host" {
		t.Fatalf("hostname=%q, err=%v", hostname, err)
	}
	if _, ok := responseShape["origin"]; ok {
		t.Fatalf("POST response flattened target identity: %s", registered.Body.String())
	}
	var target storage.RegisteredTarget
	if err := json.Unmarshal(registered.Body.Bytes(), &target); err != nil {
		t.Fatal(err)
	}
	if target.SeenAt <= 0 || target.ExpiresAt-target.SeenAt != int64(2*time.Minute/time.Millisecond) {
		t.Fatalf("registration lease shape: %+v", target)
	}
	if target.Target.Origin != *actor || target.Target.Generation != "cccccccc-cccc-4ccc-8ccc-cccccccccccc" {
		t.Fatalf("registration target did not use native origin: %+v", target.Target)
	}
	listed := taskRequest(handler, http.MethodGet, path, "", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listed.Code, listed.Body.String())
	}
	var directory storage.TargetDirectory
	if err := json.Unmarshal(listed.Body.Bytes(), &directory); err != nil || len(directory.Targets) != 1 {
		t.Fatalf("list result: %+v %v (%s)", directory, err, listed.Body.String())
	}
	var listShape map[string]json.RawMessage
	if err := json.Unmarshal(listed.Body.Bytes(), &listShape); err != nil {
		t.Fatal(err)
	}
	if _, ok := listShape["canonical_remote"]; !ok {
		t.Fatalf("GET response omitted canonical_remote: %s", listed.Body.String())
	}
	var listEntries []map[string]json.RawMessage
	if err := json.Unmarshal(listShape["targets"], &listEntries); err != nil || len(listEntries) != 1 {
		t.Fatalf("invalid target list shape: %s (%v)", listShape["targets"], err)
	}
	for _, field := range []string{"target", "seen_at", "expires_at", "display_name", "hostname"} {
		if _, ok := listEntries[0][field]; !ok {
			t.Fatalf("GET target omitted %q: %s", field, listShape["targets"])
		}
	}
	if _, ok := listEntries[0]["canonical_remote"]; ok {
		t.Fatalf("GET target included per-entry canonical_remote: %s", listShape["targets"])
	}
	if directory.Targets[0].Target != target.Target {
		t.Fatalf("list changed target: %+v != %+v", directory.Targets[0].Target, target.Target)
	}
	if _, err := store.RegisterTarget(t.Context(), "tasks", storage.TargetRegistration{
		Origin: *actor, Generation: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
	}, time.Now().UTC().Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	expired := taskRequest(handler, http.MethodGet, path, "", nil)
	if err := json.Unmarshal(expired.Body.Bytes(), &directory); err != nil || len(directory.Targets) != 0 {
		t.Fatalf("expired target remained listed: %+v %v", directory, err)
	}
}

func TestTargetRegistrationValidatesNativeOriginAndStrictBody(t *testing.T) {
	_, handler := taskAPI(t)
	path := "/v1/projects/tasks/task-targets"
	body := `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}`
	for _, test := range []struct {
		name  string
		body  string
		actor *storage.AgentOrigin
	}{
		{name: "missing origin", body: body},
		{name: "partial origin", body: body, actor: &storage.AgentOrigin{SessionID: apiActor().SessionID}},
		{name: "malformed origin", body: body, actor: &storage.AgentOrigin{SessionID: "bad", InstanceID: apiActor().InstanceID, Provider: "codex", OS: "linux"}},
		{name: "missing generation", body: `{}`, actor: apiActor()},
		{name: "invalid generation", body: `{"generation":"bad"}`, actor: apiActor()},
		{name: "unknown field", body: `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","origin":{}}`, actor: apiActor()},
		{name: "trailing JSON", body: body + `{}`, actor: apiActor()},
		{name: "null request", body: `null`, actor: apiActor()},
		{name: "oversized label", body: `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","display_name":"` + strings.Repeat("x", 129) + `"}`, actor: apiActor()},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := taskRequest(handler, http.MethodPost, path, test.body, test.actor)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer task-key")
	request.Header.Set("X-Agent-Session-Id", apiActor().SessionID)
	request.Header.Add("X-Agent-Session-Id", apiActor().SessionID)
	request.Header.Set("X-Agent-Instance-Id", apiActor().InstanceID)
	request.Header.Set("X-Agent-Provider", apiActor().Provider)
	request.Header.Set("X-Agent-OS", apiActor().OS)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate origin header status %d: %s", response.Code, response.Body.String())
	}
}

func TestTargetRegistrationBindingConflictsAndMissingProject(t *testing.T) {
	store, handler := taskAPI(t)
	remote := "github.com/acme/tasks"
	if err := store.InsertProject(t.Context(), storage.Project{Ident: "repo", ChannelName: "none", Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/repo/task-targets"
	actor := apiActor()
	valid := taskRequest(handler, http.MethodPost, path, `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","canonical_remote":"https://github.com/acme/tasks.git"}`, actor)
	if valid.Code != http.StatusOK {
		t.Fatalf("matching binding status %d: %s", valid.Code, valid.Body.String())
	}
	var registered storage.RegisteredTarget
	if err := json.Unmarshal(valid.Body.Bytes(), &registered); err != nil || registered.CanonicalRemote == nil || *registered.CanonicalRemote != remote {
		t.Fatalf("matching binding response %+v, %v", registered, err)
	}
	for _, body := range []string{
		`{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}`,
		`{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","canonical_remote":"github.com/other/repo"}`,
		`{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","canonical_remote":"not a remote"}`,
	} {
		response := taskRequest(handler, http.MethodPost, path, body, actor)
		if response.Code != http.StatusConflict {
			t.Fatalf("binding conflict status %d: %s", response.Code, response.Body.String())
		}
	}
	missing := taskRequest(handler, http.MethodPost, "/v1/projects/missing/task-targets", `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}`, actor)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing project status %d: %s", missing.Code, missing.Body.String())
	}
}
