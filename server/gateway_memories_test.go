package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestGatewayMemoryReadRoutesFilterAndDiscoverRetainedRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway-memories-http.sqlite")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, ident := range []string{"memory-demo", "retired-memory", storage.GatewayGlobalMemoryProjectIdent} {
		if err := store.InsertProject(ctx, storage.Project{Ident: ident, ChannelName: "internal", Kind: "adhoc"}); err != nil {
			t.Fatal(err)
		}
	}
	seedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seedDB.Close() })
	seed := func(id, project, content, memoryType, tags string, revision, updated int64, tombstoned any, conceptHash string, okf string) {
		t.Helper()
		_, err := seedDB.ExecContext(ctx, `INSERT INTO gateway_memories(
			id,project_ident,server_revision,content,memory_type,tags,content_hash,source_agent_id,source_hostname,
			source_client,source_client_version,provenance_json,created_at,updated_at,tombstoned_at,concept_hash,okf_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, project, revision, content, memoryType, tags, "hash-"+id,
			"agent-a", "host-a", "client-a", "v1", `{"kind":"fixture"}`, 10, updated, tombstoned, conceptHash, okf)
		if err != nil {
			t.Fatal(err)
		}
	}
	seed("live-1", "memory-demo", "agent notes", "project", `["go","gateway"]`, 1, 100, nil, "concept-1", `{"format":"okf"}`)
	seed("live-3", "memory-demo", "newest agent notes", "project", `["go"]`, 3, 300, nil, "concept-3", `{"format":"okf3"}`)
	seed("deleted-4", "memory-demo", "deleted notes", "project", `["go"]`, 4, 400, int64(450), "concept-4", `{"format":"okf4"}`)
	seed("retired-6", "retired-memory", "gone", "feedback", `["old"]`, 6, 600, int64(650), "concept-6", `{"format":"okf6"}`)
	seed("global-2", storage.GatewayGlobalMemoryProjectIdent, "global preference", "user", `["favorite"]`, 2, 200, nil, "concept-g", `{"format":"okfg"}`)
	handler, err := NewHandler(store, "memory-key")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, capabilities string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer memory-key")
		if capabilities != "" {
			req.Header.Set("X-Memory-Capabilities", capabilities)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	filtered := get("/v1/projects/MEMORY-DEMO/memories?since_revision=1", "")
	if filtered.Code != http.StatusOK {
		t.Fatalf("filtered listing status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	var listing map[string]any
	if err := json.Unmarshal(filtered.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing["project_ident"] != "memory-demo" || listing["server_revision"] != float64(4) || len(listing["memories"].([]any)) != 1 {
		t.Fatalf("revision must be project-wide and tombstones hidden by default: %#v", listing)
	}
	limited := get("/v1/projects/memory-demo/memories?limit=0", "")
	if err := json.Unmarshal(limited.Body.Bytes(), &listing); err != nil || limited.Code != http.StatusOK || len(listing["memories"].([]any)) != 1 || listing["memories"].([]any)[0].(map[string]any)["gateway_memory_id"] != "live-3" {
		t.Fatalf("explicit zero limit response status=%d result=%#v err=%v", limited.Code, listing, err)
	}
	withoutCapability := get("/v1/projects/memory-demo/memories?include_tombstones=true&since_revision=3", "")
	if err := json.Unmarshal(withoutCapability.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	memories := listing["memories"].([]any)
	deleted := memories[0].(map[string]any)
	if deleted["gateway_memory_id"] != "deleted-4" || deleted["tombstone"] != true || deleted["concept_hash"] != nil || deleted["okf"] != nil {
		t.Fatalf("tombstone/capability filtering mismatch: %#v", deleted)
	}
	withCapability := get("/v1/projects/memory-demo/memories?since_revision=2&type=PROJECT&tag=go&q=agent", "other, okf-markdown-v1")
	if err := json.Unmarshal(withCapability.Body.Bytes(), &listing); err != nil || withCapability.Code != http.StatusOK {
		t.Fatalf("capability listing status=%d body=%s err=%v", withCapability.Code, withCapability.Body.String(), err)
	}
	memories = listing["memories"].([]any)
	if len(memories) != 1 || memories[0].(map[string]any)["gateway_memory_id"] != "live-3" || memories[0].(map[string]any)["concept_hash"] != "concept-3" || memories[0].(map[string]any)["okf"] == nil {
		t.Fatalf("capability fields/filter mismatch: %#v", listing)
	}
	if missing := get("/v1/projects/not-present/memories", ""); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown project status=%d body=%s", missing.Code, missing.Body.String())
	}
	if global := get("/v1/projects/__global__/memories", ""); global.Code != http.StatusOK {
		t.Fatalf("global memory route status=%d body=%s", global.Code, global.Body.String())
	}
	pullRequest := httptest.NewRequest(http.MethodPost, "/v1/projects/memory-demo/memories/pull", strings.NewReader(`{"since_revision":2,"page_size":2,"known":[{"gateway_memory_id":"live-1","server_revision":0}]}`))
	pullRequest.Header.Set("Authorization", "Bearer memory-key")
	pullRequest.Header.Set("Content-Type", "application/json")
	pullRequest.Header.Set("X-Memory-Capabilities", "okf-markdown-v1")
	pullResponse := httptest.NewRecorder()
	handler.ServeHTTP(pullResponse, pullRequest)
	var pulled storage.GatewayMemoryPullResponse
	if pullResponse.Code != http.StatusOK || json.Unmarshal(pullResponse.Body.Bytes(), &pulled) != nil {
		t.Fatalf("pull status=%d body=%s", pullResponse.Code, pullResponse.Body.String())
	}
	if pulled.ProjectIdent != "memory-demo" || pulled.ServerRevision != 4 || pulled.SinceRevision != 2 || pulled.NextSinceRevision != 4 || pulled.HasMore || len(pulled.Memories) != 1 || pulled.Memories[0].ID != "live-3" || pulled.Memories[0].ConceptHash == nil || len(pulled.Tombstones) != 1 || pulled.Tombstones[0].GatewayMemoryID != "deleted-4" || len(pulled.Conflicts) != 1 || pulled.Conflicts[0].GatewayMemoryID != "live-1" {
		t.Fatalf("revision pull response=%+v", pulled)
	}
	pullPageRequest := httptest.NewRequest(http.MethodPost, "/v1/projects/memory-demo/memories/pull", strings.NewReader(`{"page_size":1}`))
	pullPageRequest.Header.Set("Authorization", "Bearer memory-key")
	pullPageRequest.Header.Set("Content-Type", "application/json")
	pullPageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pullPageResponse, pullPageRequest)
	if err := json.Unmarshal(pullPageResponse.Body.Bytes(), &pulled); err != nil || pullPageResponse.Code != http.StatusOK || !pulled.HasMore || pulled.NextSinceRevision != 1 || len(pulled.Memories) != 1 || pulled.Memories[0].ID != "live-1" {
		t.Fatalf("pull page response status=%d response=%+v err=%v body=%s", pullPageResponse.Code, pulled, err, pullPageResponse.Body.String())
	}
	discovery := get("/v1/memories/projects", "")
	if discovery.Code != http.StatusOK {
		t.Fatalf("discovery status=%d body=%s", discovery.Code, discovery.Body.String())
	}
	var projects storage.GatewayMemoryProjectsResponse
	if err := json.Unmarshal(discovery.Body.Bytes(), &projects); err != nil {
		t.Fatal(err)
	}
	want := []storage.GatewayMemoryProject{
		{ProjectIdent: storage.GatewayGlobalMemoryProjectIdent, MemoryCount: 1, ServerRevision: 2},
		{ProjectIdent: "memory-demo", MemoryCount: 2, ServerRevision: 4},
		{ProjectIdent: "retired-memory", MemoryCount: 0, ServerRevision: 6},
	}
	if len(projects.Projects) != len(want) {
		t.Fatalf("discovered projects=%+v", projects.Projects)
	}
	for i := range want {
		if projects.Projects[i] != want[i] {
			t.Fatalf("discovery[%d]=%+v want=%+v", i, projects.Projects[i], want[i])
		}
	}
}

func TestGatewayMemoryReadRoutesSupportFreshProfiles(t *testing.T) {
	_, handler := taskAPI(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/projects/tasks/memories", nil)
	request.Header.Set("Authorization", "Bearer task-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() == "" {
		t.Fatalf("fresh memory list status=%d body=%s", response.Code, response.Body.String())
	}
	pullRequest := httptest.NewRequest(http.MethodPost, "/v1/projects/__global__/memories/pull", strings.NewReader(`{}`))
	pullRequest.Header.Set("Authorization", "Bearer task-key")
	pullResponse := httptest.NewRecorder()
	handler.ServeHTTP(pullResponse, pullRequest)
	var pulled storage.GatewayMemoryPullResponse
	if pullResponse.Code != http.StatusOK || json.Unmarshal(pullResponse.Body.Bytes(), &pulled) != nil || pulled.ProjectIdent != storage.GatewayGlobalMemoryProjectIdent || pulled.ServerRevision != 0 || pulled.Memories == nil || pulled.Tombstones == nil || pulled.Conflicts == nil {
		t.Fatalf("fresh global pull status=%d response=%+v body=%s", pullResponse.Code, pulled, pullResponse.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/memories/projects", nil)
	request.Header.Set("Authorization", "Bearer task-key")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var discovery storage.GatewayMemoryProjectsResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &discovery) != nil || discovery.Projects == nil || len(discovery.Projects) != 0 {
		t.Fatalf("fresh memory discovery status=%d response=%+v body=%s", response.Code, discovery, response.Body.String())
	}
}

func TestGatewayMemoryPushRouteBoundsAndStructuredRejections(t *testing.T) {
	store, handler := taskAPI(t)
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer task-key")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-ID", "agent-source")
		req.Header.Set("X-Memory-Client-ID", "memory-client")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	missing := post("/v1/projects/no-such-project/memories", `{"memories":[{"local_memory_id":"local","content":"x","type":"project"}]}`)
	var rejected storage.GatewayMemoryPushResponse
	if missing.Code != http.StatusOK || json.Unmarshal(missing.Body.Bytes(), &rejected) != nil || rejected.ProjectIdent != "no-such-project" || rejected.ServerRevision != 0 || len(rejected.Results) != 1 || rejected.Results[0].Action != "rejected" || rejected.Results[0].ClientID == nil || *rejected.Results[0].ClientID != "memory-client" {
		t.Fatalf("missing project push status=%d response=%+v body=%s", missing.Code, rejected, missing.Body.String())
	}
	item := rejected.Results[0]
	if item.LocalMemoryID == nil || *item.LocalMemoryID != "local" || item.Error == nil || *item.Error != "project 'no-such-project' not found on gateway" {
		t.Fatalf("missing project push receipt: %+v", item)
	}
	project, err := store.Project(context.Background(), "no-such-project")
	if err != nil || project != nil {
		t.Fatalf("missing push created a project: %+v err=%v", project, err)
	}
	memory, err := store.SearchGatewayMemories(context.Background(), "no-such-project", storage.GatewayMemoryFilters{})
	if err != nil || memory.ServerRevision != 0 || len(memory.Memories) != 0 {
		t.Fatalf("missing push wrote history: %+v err=%v", memory, err)
	}
	tooMany := make([]string, storage.GatewayMemoryBatchLimit+1)
	for i := range tooMany {
		tooMany[i] = `{}`
	}
	oversized := post("/v1/projects/tasks/memories", `{"memories":[`+strings.Join(tooMany, ",")+"]}")
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch status=%d body=%s", oversized.Code, oversized.Body.String())
	}
	created := post("/v1/projects/tasks/memories", `{"memories":[{"local_memory_id":"local","project_ident":"  ","content":"retained","type":"project"}]}`)
	var pushed storage.GatewayMemoryPushResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &pushed) != nil || pushed.ServerRevision != 1 || len(pushed.Results) != 1 || pushed.Results[0].Action != "created" {
		t.Fatalf("valid push status=%d response=%+v body=%s", created.Code, pushed, created.Body.String())
	}

	replay := post("/v1/projects/tasks/memories/push", `{"memories":[{"local_memory_id":"local","content":"retained","type":"project"}]}`)
	var linked storage.GatewayMemoryPushResponse
	if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &linked) != nil || linked.ServerRevision != 1 || len(linked.Results) != 1 || linked.Results[0].Action != "linked" {
		t.Fatalf("push alias did not reuse the existing record: status=%d body=%s", replay.Code, replay.Body.String())
	}

}

func TestGlobalMemoryPullOnFreshProfileReturnsEmptyArrays(t *testing.T) {
	_, handler := taskAPI(t)
	const globalPull = "/v1/projects/__global__/memories/pull"
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, globalPull, nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated global pull status=%d", unauthenticated.Code)
	}
	missing := taskRequest(handler, http.MethodPost, "/v1/projects/missing/memories/pull", "{}", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing project pull status=%d body=%s", missing.Code, missing.Body.String())
	}
	response := taskRequest(handler, http.MethodPost, globalPull, "{}", nil)
	var pull storage.GatewayMemoryPullResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &pull) != nil {
		t.Fatalf("fresh global pull status=%d body=%s", response.Code, response.Body.String())
	}
	if pull.ProjectIdent != storage.GatewayGlobalMemoryProjectIdent || pull.ServerRevision != 0 || pull.SinceRevision != 0 || pull.NextSinceRevision != 0 || pull.HasMore {
		t.Fatalf("fresh global pull paging: %+v", pull)
	}
	if pull.Memories == nil || pull.Tombstones == nil || pull.Conflicts == nil || len(pull.Memories)+len(pull.Tombstones)+len(pull.Conflicts) != 0 {
		t.Fatalf("fresh global pull should return empty arrays: %s", response.Body.String())
	}
}

func TestGatewayMemoryOKFRoundTripAndValidation(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	const path = "/v1/projects/write-demo/memories"

	discovery := artifactWriteRequest(handler, http.MethodGet, "/v1/memories/capabilities", nil, nil)
	var advertised struct {
		Version      int `json:"version"`
		Capabilities []struct {
			ID               string `json:"id"`
			Version          int    `json:"version"`
			Format           string `json:"format"`
			RequestHeader    string `json:"request_header"`
			MaxEnvelopeBytes int    `json:"max_envelope_bytes"`
			MaxDocumentBytes int    `json:"max_document_bytes"`
			MaxDepth         int    `json:"max_depth"`
			MaxNodes         int    `json:"max_nodes"`
		} `json:"capabilities"`
	}
	if discovery.Code != http.StatusOK || json.Unmarshal(discovery.Body.Bytes(), &advertised) != nil || advertised.Version != 1 || len(advertised.Capabilities) != 1 {
		t.Fatalf("capability discovery status=%d body=%s", discovery.Code, discovery.Body.String())
	}
	okf := advertised.Capabilities[0]
	if okf.ID != "okf-markdown-v1" || okf.Version != 1 || okf.Format != "okf-markdown" || okf.RequestHeader != "X-Memory-Capabilities" ||
		okf.MaxEnvelopeBytes != 1048576 || okf.MaxDocumentBytes != 524288 || okf.MaxDepth != 16 || okf.MaxNodes != 16384 {
		t.Fatalf("unexpected OKF capability: %+v", okf)
	}
	hash := strings.Repeat("a", 64)
	envelope := map[string]any{
		"version": 1, "format": "okf-markdown", "revision": 1,
		"semantic_hash": hash, "document": "# Synthetic memory\n",
		"future_extension": map[string]any{"large": json.Number("123456789012345678901234567890")},
	}
	item := map[string]any{
		"local_memory_id": "okf-fixture", "memory_type": "project", "content": "Synthetic memory",
		"concept_hash": strings.Repeat("b", 64), "okf": envelope,
	}
	batch := map[string]any{"memories": []any{item}}
	rejected := artifactWriteRequest(handler, http.MethodPost, path+"/push", batch, nil)
	var rejection storage.GatewayMemoryPushResponse
	if rejected.Code != http.StatusOK || json.Unmarshal(rejected.Body.Bytes(), &rejection) != nil ||
		rejection.ServerRevision != 0 || len(rejection.Results) != 1 || rejection.Results[0].Action != "rejected" {
		t.Fatalf("invalid OKF should reject without allocating a revision: %d %s", rejected.Code, rejected.Body.String())
	}
	item["concept_hash"] = hash
	accepted := artifactWriteRequest(handler, http.MethodPost, path+"/push", batch, nil)
	var pushed storage.GatewayMemoryPushResponse
	if accepted.Code != http.StatusOK || json.Unmarshal(accepted.Body.Bytes(), &pushed) != nil ||
		pushed.ServerRevision != 1 || len(pushed.Results) != 1 || pushed.Results[0].Action != "created" {
		t.Fatalf("valid OKF should create a memory: %d %s", accepted.Code, accepted.Body.String())
	}
	pull := func(headers map[string]string) storage.GatewayMemory {
		t.Helper()
		response := artifactWriteRequest(handler, http.MethodPost, path+"/pull", map[string]any{}, headers)
		var data storage.GatewayMemoryPullResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &data) != nil || len(data.Memories) != 1 {
			t.Fatalf("OKF pull status=%d body=%s", response.Code, response.Body.String())
		}
		return data.Memories[0]
	}
	plain := pull(nil)
	if plain.ConceptHash != nil || len(plain.OKF) != 0 {
		t.Fatalf("legacy pull exposed optional envelope: %+v", plain)
	}
	withOKF := pull(map[string]string{okf.RequestHeader: okf.ID})
	var stored map[string]any
	if withOKF.ConceptHash == nil || *withOKF.ConceptHash != hash || json.Unmarshal(withOKF.OKF, &stored) != nil ||
		stored["document"] != envelope["document"] || stored["semantic_hash"] != hash || stored["revision"] != float64(1) || !strings.Contains(string(withOKF.OKF), "123456789012345678901234567890") {
		t.Fatalf("negotiated pull changed the envelope: %+v", withOKF)
	}

	checkPush := func(action string, revision int64) {
		t.Helper()
		response := artifactWriteRequest(handler, http.MethodPost, path+"/push", batch, nil)
		var result storage.GatewayMemoryPushResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
			result.ServerRevision != revision || len(result.Results) != 1 || result.Results[0].Action != action {
			t.Fatalf("expected %s at revision %d: status=%d body=%s", action, revision, response.Code, response.Body.String())
		}
	}
	item["client_id"], item["local_memory_id"] = "second-client", "second-local"
	checkPush("linked", 1)
	item["gateway_memory_id"] = withOKF.ID
	item["base_gateway_revision"] = 1
	envelope["future_extension"].(map[string]any)["extra"] = []int{1, 2, 3}
	checkPush("updated", 2)
	capabilities := map[string]string{okf.RequestHeader: okf.ID}
	expectedEnvelope, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if updated := pull(capabilities); string(updated.OKF) != string(expectedEnvelope) {
		t.Fatalf("updated extension changed: %s", updated.OKF)
	}
	envelope["revision"] = 2
	checkPush("conflict", 2)
	delete(item, "base_gateway_revision")
	checkPush("conflict", 2)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "other-memory", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	foreign := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/other-memory/memories/push", batch, nil)
	var foreignResult storage.GatewayMemoryPushResponse
	if foreign.Code != http.StatusOK || json.Unmarshal(foreign.Body.Bytes(), &foreignResult) != nil ||
		foreignResult.ServerRevision != 0 || len(foreignResult.Results) != 1 || foreignResult.Results[0].Action != "rejected" {
		t.Fatalf("foreign project accessed explicit ID: status=%d body=%s", foreign.Code, foreign.Body.String())
	}
	delete(item, "okf")
	delete(item, "concept_hash")
	item["base_gateway_revision"] = 1
	checkPush("linked", 2)
	item["base_gateway_revision"] = 2
	item["tags"] = []string{"legacy-edit"}
	checkPush("updated", 3)
	afterTagEdit := pull(capabilities)
	if len(afterTagEdit.OKF) == 0 || afterTagEdit.ConceptHash == nil {
		t.Fatalf("legacy tag edit erased the envelope: %+v", afterTagEdit)
	}
	item["base_gateway_revision"], item["content"] = 3, "Changed by legacy client"
	checkPush("updated", 4)
	afterBodyEdit := pull(capabilities)
	if len(afterBodyEdit.OKF) != 0 || afterBodyEdit.ConceptHash != nil {
		t.Fatalf("legacy body edit preserved stale OKF metadata: %+v", afterBodyEdit)
	}

	item["base_gateway_revision"], item["tombstone"] = 4, true
	checkPush("tombstoned", 5)
	checkPush("conflict", 5)
	item["base_gateway_revision"] = 5
	checkPush("linked", 5)
	response := artifactWriteRequest(handler, http.MethodPost, path+"/pull", map[string]any{}, capabilities)
	var deleted storage.GatewayMemoryPullResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &deleted) != nil || deleted.ServerRevision != 5 ||
		len(deleted.Memories) != 0 || len(deleted.Tombstones) != 1 || deleted.Tombstones[0].GatewayMemoryID != withOKF.ID {
		t.Fatalf("unexpected tombstone after replay: status=%d body=%s", response.Code, response.Body.String())
	}

}
