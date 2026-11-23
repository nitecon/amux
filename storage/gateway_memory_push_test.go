package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayMemoryPushRetainsRecordsLinksAndRevisionOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway-memory-push.sqlite")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, Project{Ident: "push-demo", ChannelName: "internal", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	source := GatewayMemoryPushSource{AgentID: strPtr("agent-a"), Hostname: strPtr("host-a"), ClientID: strPtr("client-a"), ClientVersion: strPtr("1.2")}
	push := func(items ...GatewayMemoryPushItem) *GatewayMemoryPushResponse {
		t.Helper()
		got, err := store.PushGatewayMemories(ctx, "push-demo", source, items)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	item := GatewayMemoryPushItem{LocalMemoryID: strPtr("local-1"), ProjectIdent: strPtr("  "), Content: strPtr("retain this memory"), MemoryType: strPtr("project"), Tags: &[]string{"first", "first", " second "}}
	created := push(item)
	if created.ServerRevision != 1 || created.Results[0].Action != "created" {
		t.Fatalf("create=%+v", created)
	}
	absentDelete := GatewayMemoryPushItem{
		LocalMemoryID: strPtr("missing-local"), GatewayMemoryID: strPtr("already-absent"),
		BaseRevision: int64Ptr(99), ContentHash: strPtr("local-content-hash"), Tombstone: []byte(`true`),
	}
	for range 2 {
		response := push(absentDelete)
		result := response.Results[0]
		if response.ServerRevision != 1 || result.Action != "tombstoned" || result.Error != nil ||
			result.ServerRevision == nil || *result.ServerRevision != 1 ||
			result.GatewayMemoryID == nil || *result.GatewayMemoryID != "already-absent" ||
			result.LocalMemoryID == nil || *result.LocalMemoryID != "missing-local" ||
			result.ContentHash == nil || *result.ContentHash != "local-content-hash" {
			t.Fatalf("absent tombstone receipt: %+v", response)
		}
	}
	unknownWrite := push(GatewayMemoryPushItem{GatewayMemoryID: strPtr("already-absent"), Content: strPtr("must not create"), MemoryType: strPtr("project")})
	result := unknownWrite.Results[0]
	if unknownWrite.ServerRevision != 1 || result.Action != "rejected" || result.Error == nil || *result.Error != "gateway_memory_id was not found for this project" {
		t.Fatalf("ordinary push to missing ID: %+v", unknownWrite)
	}
	id := *created.Results[0].GatewayMemoryID
	linked := push(item)
	if linked.ServerRevision != 1 || linked.Results[0].Action != "linked" {
		t.Fatalf("repeat push=%+v", linked)
	}
	update := item
	update.GatewayMemoryID = &id
	update.BaseRevision = int64Ptr(1)
	update.Content = strPtr("retained edited memory")
	updated := push(update)
	if updated.ServerRevision != 2 || updated.Results[0].Action != "updated" {
		t.Fatalf("update=%+v", updated)
	}
	stale := update
	stale.BaseRevision = int64Ptr(1)
	stale.Content = strPtr("stale competing edit")
	conflict := push(stale)
	if conflict.ServerRevision != 2 || conflict.Results[0].Action != "conflict" || conflict.Results[0].Conflict == nil || conflict.Results[0].Conflict.RemoteServerRevision != 2 {
		t.Fatalf("conflict=%+v", conflict)
	}
	badScope := item
	badScope.Scope = strPtr("global")
	badProject := item
	badProject.ProjectIdent = strPtr("other")
	for _, bad := range []GatewayMemoryPushItem{badScope, badProject} {
		if got := push(bad); got.Results[0].Action != "rejected" {
			t.Fatalf("invalid item result=%+v", got.Results[0])
		}
	}
	deleted := GatewayMemoryPushItem{GatewayMemoryID: &id, LocalMemoryID: strPtr("local-1"), BaseRevision: int64Ptr(2), Tombstone: []byte(`{"deleted":true,"deleted_at":123,"reason":"forget"}`)}
	tombstoned := push(deleted)
	if tombstoned.ServerRevision != 3 || tombstoned.Results[0].Action != "tombstoned" {
		t.Fatalf("tombstone=%+v", tombstoned)
	}
	staleDelete := push(deleted)
	if staleDelete.ServerRevision != 3 || staleDelete.Results[0].Action != "conflict" {
		t.Fatalf("stale tombstone retry=%+v", staleDelete)
	}
	deleted.BaseRevision = int64Ptr(3)
	linkedDelete := push(deleted)
	if linkedDelete.ServerRevision != 3 || linkedDelete.Results[0].Action != "linked" {
		t.Fatalf("repeat tombstone=%+v", linkedDelete)
	}
	pulled, err := store.PullGatewayMemories(ctx, "push-demo", GatewayMemoryPullRequest{IncludeTombstones: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulled.Memories) != 0 || len(pulled.Tombstones) != 1 || pulled.Tombstones[0].GatewayMemoryID != id || pulled.ServerRevision != 3 {
		t.Fatalf("retained tombstone=%+v", pulled)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	replayed, err := store.PushGatewayMemories(ctx, "push-demo", source, []GatewayMemoryPushItem{item})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ServerRevision != 3 || replayed.Results[0].Action != "conflict" {
		t.Fatalf("retained tombstone replay=%+v", replayed)
	}
}

func TestGatewayMemoryPushGlobalAndMissingProjectResults(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway-memory-global.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	item := GatewayMemoryPushItem{LocalMemoryID: strPtr("global-local"), Scope: strPtr("global"), Content: strPtr("user preference"), MemoryType: strPtr("preference")}
	response, err := store.PushGatewayMemories(ctx, GatewayGlobalMemoryProjectIdent, GatewayMemoryPushSource{}, []GatewayMemoryPushItem{item})
	if err != nil {
		t.Fatal(err)
	}
	if response.ServerRevision != 1 || response.Results[0].Action != "created" {
		t.Fatalf("global push=%+v", response)
	}
	missing, err := store.PushGatewayMemories(ctx, "not-present", GatewayMemoryPushSource{ClientID: strPtr("source-client")}, []GatewayMemoryPushItem{{LocalMemoryID: strPtr("local")}})
	if err != nil {
		t.Fatal(err)
	}
	if missing.ServerRevision != 0 || missing.Results[0].Action != "rejected" || missing.Results[0].ClientID == nil || *missing.Results[0].ClientID != "source-client" {
		t.Fatalf("missing project response=%+v", missing)
	}
}

func TestGatewayMemoryPushAcceptsBooleanAndObjectTombstones(t *testing.T) {
	for _, fixture := range []struct {
		body    string
		deleted bool
	}{
		{`{"tombstone":true}`, true},
		{`{"tombstone":{"deleted":true,"deleted_at":123,"reason":"forget"}}`, true},
		{`{"tombstone":false}`, false},
		{`{}`, false},
	} {
		var item GatewayMemoryPushItem
		if err := json.Unmarshal([]byte(fixture.body), &item); err != nil || item.tombstoneRequested() != fixture.deleted {
			t.Fatalf("tombstone %s decoded=%+v err=%v", fixture.body, item, err)
		}
	}
}

func TestGatewayMemoryPushRejectsMalformedOKFBatchWithoutWrites(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway-memory-invalid-okf.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, Project{Ident: "okf-invalid", ChannelName: "internal", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	base := map[string]any{"version": 1, "format": "okf-markdown", "revision": uint64(1), "semantic_hash": hash, "document": "# Fixture"}
	bad := make([]map[string]any, 0)
	add := func(mutate func(map[string]any)) {
		item := make(map[string]any, len(base)+1)
		for key, value := range base {
			item[key] = value
		}
		mutate(item)
		bad = append(bad, item)
	}
	add(func(v map[string]any) { v["version"] = 2 })
	add(func(v map[string]any) { v["version"] = "1" })
	add(func(v map[string]any) { v["format"] = "executable" })
	add(func(v map[string]any) { v["revision"] = 0 })
	add(func(v map[string]any) { v["revision"] = 1.5 })
	add(func(v map[string]any) { v["semantic_hash"] = "invalid" })
	add(func(v map[string]any) { v["document"] = []string{} })
	add(func(v map[string]any) { v["document"] = "nul\x00text" })
	add(func(v map[string]any) { v["document"] = strings.Repeat("x", (512<<10)+1) })
	add(func(v map[string]any) { delete(v, "document") })
	add(func(v map[string]any) { v["extension"] = strings.Repeat("x", 1<<20) })
	wide := make([]bool, 16384)
	add(func(v map[string]any) { v["extension"] = wide })
	deep := []byte("null")
	for range 16 {
		deep = append(append([]byte{'['}, deep...), ']')
	}
	add(func(v map[string]any) { v["extension"] = json.RawMessage(deep) })
	items := make([]GatewayMemoryPushItem, 0, len(bad)+4)
	for i, envelope := range bad {
		raw, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, GatewayMemoryPushItem{LocalMemoryID: strPtr(fmt.Sprintf("invalid-%d", i)), Content: strPtr("fixture"), MemoryType: strPtr("project"), ConceptHash: &hash, OKF: raw})
	}
	validEnvelope, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	items = append(items,
		GatewayMemoryPushItem{LocalMemoryID: strPtr("wrong-concept"), Content: strPtr("fixture"), MemoryType: strPtr("project"), ConceptHash: strPtr(strings.Repeat("b", 64)), OKF: validEnvelope},
		GatewayMemoryPushItem{LocalMemoryID: strPtr("wrong-root"), Content: strPtr("fixture"), MemoryType: strPtr("project"), OKF: json.RawMessage(`[]`)},
		GatewayMemoryPushItem{LocalMemoryID: strPtr("wrong-scope"), Content: strPtr("fixture"), MemoryType: strPtr("project"), Scope: strPtr("global"), ConceptHash: &hash, OKF: validEnvelope},
		GatewayMemoryPushItem{LocalMemoryID: strPtr("working-context"), Content: strPtr("fixture"), MemoryType: strPtr("working_context"), ConceptHash: &hash, OKF: validEnvelope},
	)
	response, err := store.PushGatewayMemories(ctx, "okf-invalid", GatewayMemoryPushSource{ClientID: strPtr("client")}, items)
	if err != nil {
		t.Fatal(err)
	}
	if response.ServerRevision != 0 || len(response.Results) != len(items) {
		t.Fatalf("rejected batch response=%+v", response)
	}
	for i, result := range response.Results {
		if result.Action != "rejected" {
			t.Errorf("item %d accepted: %+v", i, result)
		}
	}
	for table, want := range map[string]int{"gateway_memories": 0, "gateway_memory_client_links": 0} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("%s count=%d want=%d", table, count, want)
		}
	}
}

func strPtr(value string) *string { return &value }
func int64Ptr(value int64) *int64 { return &value }
