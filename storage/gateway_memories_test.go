package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGatewayMemorySearchAndDiscoveryReadRetainedRows(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "gateway-memories.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, ident := range []string{"alpha", "tomb-only", GatewayGlobalMemoryProjectIdent} {
		if err := s.InsertProject(ctx, fixtureProject(ident)); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(id, project string, revision, updated int64, tombstoned any, content, memoryType, tags, hash string) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, `INSERT INTO gateway_memories(
			id,project_ident,server_revision,content,memory_type,tags,content_hash,source_agent_id,source_hostname,
			source_client,source_client_version,provenance_json,client_created_at,client_updated_at,created_at,updated_at,
			tombstoned_at,concept_hash,okf_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, project, revision, content, memoryType, tags, hash, "agent-x", "host-a", "client-a", "1.0",
			`{"source":"fixture"}`, nil, nil, 10, updated, tombstoned, "concept-hash", `{"format":"okf"}`)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("alpha-r1", "alpha", 1, 100, nil, "older memory", "project", `["go","storage"]`, "hash-1")
	insert("alpha-r2", "alpha", 2, 300, nil, "newer tied memory", "project", `["go"]`, "hash-2")
	insert("alpha-r3", "alpha", 3, 300, nil, "latest tied memory", "project", `["rust"]`, "hash-3")
	insert("alpha-deleted", "alpha", 4, 400, int64(450), "deleted memory", "project", `["go"]`, "hash-4")
	insert("only-deleted", "tomb-only", 7, 700, int64(750), "gone", "feedback", `["old"]`, "hash-7")
	insert("global-r2", GatewayGlobalMemoryProjectIdent, 2, 200, nil, "global preference", "user", `["favorite"]`, "hash-g")

	response, err := s.SearchGatewayMemories(ctx, "ALPHA", GatewayMemoryFilters{SinceRevision: int64Pointer(2), MemoryType: " PROJECT ", Tag: "rust"})
	if err != nil || response.ProjectIdent != "alpha" || response.ServerRevision != 4 || len(response.Memories) != 1 || response.Memories[0].ID != "alpha-r3" {
		t.Fatalf("filtered memory search=%+v err=%v", response, err)
	}
	zero := uint64(0)
	one, err := s.SearchGatewayMemories(ctx, "alpha", GatewayMemoryFilters{Limit: &zero})
	if err != nil || len(one.Memories) != 1 || one.Memories[0].ID != "alpha-r3" {
		t.Fatalf("explicit zero limit should select one newest memory: %+v err=%v", one, err)
	}
	ordered, err := s.SearchGatewayMemories(ctx, "alpha", GatewayMemoryFilters{})
	if err != nil || ordered.ServerRevision != 4 || len(ordered.Memories) != 3 {
		t.Fatalf("default memory search=%+v err=%v", ordered, err)
	}
	gotOrder := []string{ordered.Memories[0].ID, ordered.Memories[1].ID, ordered.Memories[2].ID}
	if !reflect.DeepEqual(gotOrder, []string{"alpha-r3", "alpha-r2", "alpha-r1"}) {
		t.Fatalf("updated_at/server_revision order=%v", gotOrder)
	}
	withTombstones, err := s.SearchGatewayMemories(ctx, "alpha", GatewayMemoryFilters{SinceRevision: int64Pointer(3), IncludeTombstones: true})
	if err != nil || withTombstones.ServerRevision != 4 || len(withTombstones.Memories) != 1 || withTombstones.Memories[0].ID != "alpha-deleted" || !withTombstones.Memories[0].Tombstone {
		t.Fatalf("tombstone search=%+v err=%v", withTombstones, err)
	}
	if _, err := s.SearchGatewayMemories(ctx, "missing", GatewayMemoryFilters{}); err != nil {
		t.Fatalf("storage read should not impose project existence: %v", err)
	}

	discovery, err := s.DiscoverGatewayMemoryProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Projects) != 3 {
		t.Fatalf("memory project discovery=%+v", discovery)
	}
	want := []GatewayMemoryProject{
		{ProjectIdent: GatewayGlobalMemoryProjectIdent, MemoryCount: 1, ServerRevision: 2},
		{ProjectIdent: "alpha", MemoryCount: 3, ServerRevision: 4},
		{ProjectIdent: "tomb-only", MemoryCount: 0, ServerRevision: 7},
	}
	if !reflect.DeepEqual(discovery.Projects, want) {
		t.Fatalf("discovery projects=%+v want=%+v", discovery.Projects, want)
	}
}

func TestGatewayMemoryFreshProfileHasEmptyReadResults(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "fresh-gateway-memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.InsertProject(ctx, fixtureProject("fresh")); err != nil {
		t.Fatal(err)
	}
	response, err := s.SearchGatewayMemories(ctx, "fresh", GatewayMemoryFilters{})
	if err != nil || response.ProjectIdent != "fresh" || response.ServerRevision != 0 || response.Memories == nil || len(response.Memories) != 0 {
		t.Fatalf("fresh memory listing=%+v err=%v", response, err)
	}
	discovery, err := s.DiscoverGatewayMemoryProjects(ctx)
	if err != nil || discovery.Projects == nil || len(discovery.Projects) != 0 {
		t.Fatalf("fresh memory discovery=%+v err=%v", discovery, err)
	}
}

func TestGatewayMemoryPullPagesRevisionsAndSeparatesTombstones(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "gateway-memory-pull.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, ident := range []string{"pull", GatewayGlobalMemoryProjectIdent} {
		if err := s.InsertProject(ctx, fixtureProject(ident)); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		id, project, content, hash string
		revision, updated          int64
		tombstoned                 any
	}{
		{"r1", "pull", "one", "h1", 1, 10, nil},
		{"r2", "pull", "two", "h2", 2, 20, int64(25)},
		{"r3", "pull", "three", "h3", 3, 30, nil},
		{"r4", "pull", "four", "h4", 4, 40, int64(45)},
		{"global1", GatewayGlobalMemoryProjectIdent, "global", "hg", 1, 10, nil},
	} {
		_, err := s.db.ExecContext(ctx, `INSERT INTO gateway_memories(id,project_ident,server_revision,content,memory_type,tags,content_hash,created_at,updated_at,tombstoned_at)
			VALUES(?,?,?,?,'project','[]',?,1,?,?)`, item.id, item.project, item.revision, item.content, item.hash, item.updated, item.tombstoned)
		if err != nil {
			t.Fatal(err)
		}
	}
	onePage, err := s.PullGatewayMemories(ctx, "PULL", GatewayMemoryPullRequest{
		PageSize: uint64Pointer(2), IncludeTombstones: true,
		Known: []GatewayMemoryKnownRevision{{GatewayMemoryID: "r1", ServerRevision: 0}, {GatewayMemoryID: "r3", ServerRevision: 2}, {GatewayMemoryID: "missing", ServerRevision: 0}},
	})
	if err != nil || onePage.ProjectIdent != "pull" || onePage.ServerRevision != 4 || onePage.SinceRevision != 0 || onePage.NextSinceRevision != 2 || !onePage.HasMore {
		t.Fatalf("first memory pull page=%+v err=%v", onePage, err)
	}
	if len(onePage.Memories) != 1 || onePage.Memories[0].ID != "r1" || len(onePage.Tombstones) != 1 || onePage.Tombstones[0].GatewayMemoryID != "r2" || len(onePage.Conflicts) != 2 {
		t.Fatalf("memory pull page shape=%+v", onePage)
	}
	if onePage.Conflicts[0].Reason != "remote revision moved since base" || onePage.Conflicts[1].RemoteServerRevision != 3 {
		t.Fatalf("memory pull conflicts=%+v", onePage.Conflicts)
	}
	secondPage, err := s.PullGatewayMemories(ctx, "pull", GatewayMemoryPullRequest{SinceRevision: int64Pointer(onePage.NextSinceRevision), PageSize: uint64Pointer(2), IncludeTombstones: true})
	if err != nil || secondPage.HasMore || secondPage.NextSinceRevision != 4 || len(secondPage.Memories) != 1 || len(secondPage.Tombstones) != 1 {
		t.Fatalf("second memory pull page=%+v err=%v", secondPage, err)
	}
	withoutTombstones, err := s.PullGatewayMemories(ctx, "pull", GatewayMemoryPullRequest{IncludeTombstones: false})
	if err != nil || withoutTombstones.HasMore || len(withoutTombstones.Memories) != 2 || len(withoutTombstones.Tombstones) != 0 || withoutTombstones.NextSinceRevision != 3 {
		t.Fatalf("memory pull without tombstones=%+v err=%v", withoutTombstones, err)
	}
	negativeSince := int64(-5)
	clamped, err := s.PullGatewayMemories(ctx, "pull", GatewayMemoryPullRequest{SinceRevision: &negativeSince, PageSize: uint64Pointer(1), IncludeTombstones: true})
	if err != nil || clamped.SinceRevision != 0 || clamped.NextSinceRevision != 1 || !clamped.HasMore {
		t.Fatalf("negative since_revision should clamp to zero: %+v err=%v", clamped, err)
	}
	zeroPage, err := s.PullGatewayMemories(ctx, GatewayGlobalMemoryProjectIdent, GatewayMemoryPullRequest{PageSize: uint64Pointer(0), IncludeTombstones: true})
	if err != nil || zeroPage.ProjectIdent != GatewayGlobalMemoryProjectIdent || len(zeroPage.Memories) != 1 || zeroPage.NextSinceRevision != 1 || zeroPage.HasMore {
		t.Fatalf("zero-sized global pull should clamp to one: %+v err=%v", zeroPage, err)
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }

func TestGatewayMemoryPushTreatsBlankItemProjectAsOmitted(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "memory-push-project.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InsertProject(ctx, fixtureProject("alpha")); err != nil {
		t.Fatal(err)
	}
	blankProject, foreignProject := "  ", "beta"
	content, memoryType := "fixture memory", "project"
	response, err := store.PushGatewayMemories(ctx, "alpha", GatewayMemoryPushSource{}, []GatewayMemoryPushItem{
		{ProjectIdent: &blankProject, Content: &content, MemoryType: &memoryType},
		{ProjectIdent: &foreignProject, Content: &content, MemoryType: &memoryType},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || response.Results[0].GatewayMemoryID == nil || response.Results[1].Action != "rejected" || response.ServerRevision != 1 {
		t.Fatalf("blank project should permit one write, foreign project should reject: %+v", response)
	}
}

func TestGatewayMemoryOKFRequiresExactUnsignedIntegers(t *testing.T) {
	for _, tc := range []struct {
		name, version, revision string
		valid                   bool
	}{
		{"max unsigned revision", "1", "18446744073709551615", true},
		{"overflowing revision", "1", "18446744073709551616", false},
		{"decimal revision", "1", "1.0", false},
		{"exponent revision", "1", "1e0", false},
		{"zero revision", "1", "0", false},
		{"negative revision", "1", "-1", false},
		{"decimal version", "1.0", "1", false},
		{"unsupported version", "2", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := `{"version":` + tc.version + `,"revision":` + tc.revision +
				`,"format":"okf-markdown","semantic_hash":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","document":"# Fixture"}`
			err := validateGatewayMemoryOKF(GatewayMemoryPushItem{OKF: []byte(envelope)})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, got %v", tc.valid, err)
			}
		})
	}
	null := GatewayMemoryPushItem{OKF: []byte(`null`)}
	if err := validateGatewayMemoryOKF(null); err != nil || null.okfPresent() {
		t.Fatalf("explicit null should behave as an absent optional envelope: %v", err)
	}
}
