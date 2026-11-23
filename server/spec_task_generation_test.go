package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestSpecTaskGenerationUsesAcceptedVersionAndReplaysStableIDs(t *testing.T) {
	store, handler, artifactID, acceptedVersionID, currentVersionID, _ := acceptedSpecTaskFixture(t, true)
	path := "/v1/projects/write-demo/specs/" + artifactID + "/generate-tasks"
	body := map[string]any{"confirmed": true, "reporter": "codex-test", "hostname": "test-host"}
	headers := specTaskOriginHeaders("generate-accepted-version")
	first := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("generation status=%d body=%s", first.Code, first.Body.String())
	}
	firstValue := artifactWriteJSON(t, first)
	firstData := firstValue["data"].(map[string]any)
	if firstData["artifact_version_id"] != acceptedVersionID || firstData["replayed"] != false {
		t.Fatalf("generation did not select accepted source: %#v", firstData)
	}
	taskIDs := firstData["generated_task_ids"].([]any)
	linkIDs := firstData["generated_link_ids"].([]any)
	if len(taskIDs) != 2 || len(linkIDs) != 2 {
		t.Fatalf("expected two tasks and links, got %#v", firstData)
	}
	taskID := taskIDs[0].(string)
	detail, err := store.Task(context.Background(), "write-demo", taskID)
	if err != nil || detail == nil || detail.Task.Details == nil || !strings.Contains(*detail.Task.Details, "source_spec_version_id: "+acceptedVersionID) || strings.Contains(*detail.Task.Details, currentVersionID) || !strings.HasPrefix(detail.Task.Title, "T001 Original") {
		t.Fatalf("task did not retain accepted-version source: %+v, %v", detail, err)
	}
	if len(detail.Comments) != 1 || !strings.Contains(detail.Comments[0].Content, "current task schema has no dedicated source fields") {
		t.Fatalf("generated task audit comment missing: %+v", detail.Comments)
	}
	links, err := store.ListArtifactLinks(context.Background(), "write-demo", storage.ArtifactLinkFilter{
		LinkType: "task_generated_from_spec", SourceKind: "artifact_version", SourceID: acceptedVersionID, TargetKind: "task", TargetID: taskID,
	})
	if err != nil || len(links) != 1 || links[0].LinkID != linkIDs[0] || links[0].SourceChildAddress == nil || *links[0].SourceChildAddress != "manifest.items[phase-1:T001]" || links[0].TargetVersionID == nil || *links[0].TargetVersionID != acceptedVersionID {
		t.Fatalf("generated artifact link mismatch: %+v, %v", links, err)
	}
	replay := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
	replayValue := artifactWriteJSON(t, replay)
	replayData := replayValue["data"].(map[string]any)
	if replay.Code != http.StatusOK || replayValue["provenance"].(map[string]any)["replay"] != true ||
		!slices.Equal(replayData["generated_task_ids"].([]any), taskIDs) || !slices.Equal(replayData["generated_link_ids"].([]any), linkIDs) {
		t.Fatalf("generation replay changed stable IDs: status=%d body=%s", replay.Code, replay.Body.String())
	}
	runID := firstData["workflow_run_id"].(string)
	run, err := store.WorkflowRun(context.Background(), runID)
	if err != nil || run == nil || run.State != "succeeded" || run.SourceArtifactVersionID == nil || *run.SourceArtifactVersionID != acceptedVersionID ||
		!slices.Equal(run.GeneratedTaskIDs, []string{taskIDs[0].(string), taskIDs[1].(string)}) ||
		!slices.Equal(run.GeneratedLinkIDs, []string{linkIDs[0].(string), linkIDs[1].(string)}) {
		t.Fatalf("workflow run output mismatch: %+v, %v", run, err)
	}
	allTasks, err := store.Tasks(context.Background(), "write-demo", []string{"todo", "in_progress", "user_review", "done", "cancelled"}, true)
	if err != nil || len(allTasks) != 2 {
		t.Fatalf("replay created duplicate tasks: count=%d err=%v", len(allTasks), err)
	}
}

func TestSpecTaskGenerationRecoversAnUnlinkedSourceTask(t *testing.T) {
	store, handler, artifactID, acceptedVersionID, _, _ := acceptedSpecTaskFixture(t, false)
	similar := taskTestManifestItem("phase-1:T0010", "T0010 Distinct item")
	similarDetails := generatedSpecTaskDetails(artifactID, acceptedVersionID, similar)
	if _, err := store.InsertTask(t.Context(), storage.TaskInsert{
		Project: "write-demo", Title: similar.Title, Details: &similarDetails, Labels: []string{"generated-from-spec"}, Reporter: "fixture",
	}, nil); err != nil {
		t.Fatal(err)
	}
	item := taskTestManifestItem("phase-1:T001", "T001 Recovered")
	details := generatedSpecTaskDetails(artifactID, acceptedVersionID, item)
	orphan, err := store.InsertTask(context.Background(), storage.TaskInsert{
		Project: "write-demo", Title: "T001 Recovered", Details: &details, Labels: []string{"generated-from-spec"}, Reporter: "fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/write-demo/specs/" + artifactID + "/generate-tasks"
	response := artifactWriteRequest(handler, http.MethodPost, path, map[string]any{
		"confirmed": true, "manifest_item_ids": []string{"phase-1:T001"},
	}, specTaskOriginHeaders("recover-unlinked-source-task"))
	if response.Code != http.StatusCreated {
		t.Fatalf("generation status=%d body=%s", response.Code, response.Body.String())
	}
	data := artifactWriteJSON(t, response)["data"].(map[string]any)
	if data["generated_task_ids"].([]any)[0] != orphan.ID {
		t.Fatalf("unlinked source task was duplicated: %#v", data)
	}
	allTasks, err := store.Tasks(context.Background(), "write-demo", []string{"todo", "in_progress", "user_review", "done", "cancelled"}, true)
	if err != nil || len(allTasks) != 2 {
		t.Fatalf("source task recovery left duplicates: count=%d err=%v", len(allTasks), err)
	}
}

func TestSpecTaskGenerationConcurrentSameKeyHTTPReplayUsesStableIDs(t *testing.T) {
	store, handler, artifactID, _, _, _ := acceptedSpecTaskFixture(t, false)
	path := "/v1/projects/write-demo/specs/" + artifactID + "/generate-tasks"
	body := map[string]any{"confirmed": true, "reporter": "codex-test", "hostname": "test-host"}
	headers := specTaskOriginHeaders("concurrent-generate-same-key")
	const callers = 8
	responses := make([]*httptest.ResponseRecorder, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses[i] = artifactWriteRequest(handler, http.MethodPost, path, body, headers)
		}(i)
	}
	wg.Wait()

	var taskIDs, linkIDs []any
	for i, response := range responses {
		if response.Code != http.StatusCreated && response.Code != http.StatusOK {
			t.Fatalf("concurrent request %d status=%d body=%s", i, response.Code, response.Body.String())
		}
		data := artifactWriteJSON(t, response)["data"].(map[string]any)
		gotTasks, gotLinks := data["generated_task_ids"].([]any), data["generated_link_ids"].([]any)
		if taskIDs == nil {
			taskIDs, linkIDs = gotTasks, gotLinks
			continue
		}
		if !slices.Equal(gotTasks, taskIDs) || !slices.Equal(gotLinks, linkIDs) {
			t.Fatalf("concurrent replay %d changed generated IDs: tasks=%v links=%v, expected tasks=%v links=%v", i, gotTasks, gotLinks, taskIDs, linkIDs)
		}
	}
	allTasks, err := store.Tasks(context.Background(), "write-demo", []string{"todo", "in_progress", "user_review", "done", "cancelled"}, true)
	if err != nil || len(allTasks) != len(taskIDs) {
		t.Fatalf("concurrent replay left duplicate tasks: count=%d expected=%d err=%v", len(allTasks), len(taskIDs), err)
	}
	for _, taskID := range taskIDs {
		detail, err := store.Task(context.Background(), "write-demo", taskID.(string))
		if err != nil || detail == nil || len(detail.Comments) != 1 {
			t.Fatalf("concurrent replay duplicated task audit comment for %v: detail=%+v err=%v", taskID, detail, err)
		}
	}
}

func TestSpecTaskGenerationConcurrentDistinctKeysReuseSourceItemLink(t *testing.T) {
	store, handler, artifactID, _, _, _ := acceptedSpecTaskFixture(t, false)
	path := "/v1/projects/write-demo/specs/" + artifactID + "/generate-tasks"
	body := map[string]any{"confirmed": true, "manifest_item_ids": []string{"phase-1:T001"}, "reporter": "codex-test"}
	const callers = 6
	responses := make([]*httptest.ResponseRecorder, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			headers := specTaskOriginHeaders("generate-distinct-operation-" + strconv.Itoa(i))
			responses[i] = artifactWriteRequest(handler, http.MethodPost, path, body, headers)
		}(i)
	}
	wg.Wait()

	var taskID, linkID string
	for i, response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("distinct-key request %d status=%d body=%s", i, response.Code, response.Body.String())
		}
		data := artifactWriteJSON(t, response)["data"].(map[string]any)
		gotTasks, gotLinks := data["generated_task_ids"].([]any), data["generated_link_ids"].([]any)
		if len(gotTasks) != 1 || len(gotLinks) != 1 {
			t.Fatalf("request %d outputs=%#v", i, data)
		}
		if taskID == "" {
			taskID, linkID = gotTasks[0].(string), gotLinks[0].(string)
		} else if gotTasks[0] != taskID || gotLinks[0] != linkID {
			t.Fatalf("distinct keys created duplicate source output: tasks=%v links=%v want task=%s link=%s", gotTasks, gotLinks, taskID, linkID)
		}
	}
	allTasks, err := store.Tasks(context.Background(), "write-demo", []string{"todo", "in_progress", "user_review", "done", "cancelled"}, true)
	if err != nil || len(allTasks) != 1 {
		t.Fatalf("distinct keys created duplicate tasks: count=%d err=%v", len(allTasks), err)
	}
	links, err := store.ListArtifactLinks(context.Background(), "write-demo", storage.ArtifactLinkFilter{
		LinkType: "task_generated_from_spec", SourceKind: "artifact_version", TargetKind: "task",
	})
	if err != nil || len(links) != 1 || links[0].LinkID != linkID {
		t.Fatalf("distinct keys created duplicate source links: links=%+v err=%v", links, err)
	}
}

func TestLinkExistingSpecTaskWritesStableBackLink(t *testing.T) {
	store, handler, artifactID, acceptedVersionID, _, actorID := acceptedSpecTaskFixture(t, false)
	task, err := store.InsertTask(context.Background(), storage.TaskInsert{
		Project: "write-demo", Title: "Existing implementation task", Reporter: "fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/write-demo/specs/" + artifactID + "/link-task"
	body := map[string]string{"manifest_item_id": "phase-1:T002", "task_id": task.ID}
	headers := map[string]string{"Idempotency-Key": "link-existing-spec-task"}
	first := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("link status=%d body=%s", first.Code, first.Body.String())
	}
	firstData := artifactWriteJSON(t, first)["data"].(map[string]any)
	if firstData["target_id"] != task.ID || firstData["source_child_address"] != "manifest.items[phase-1:T002]" || firstData["source_version_id"] != acceptedVersionID {
		t.Fatalf("unexpected spec back link: %#v", firstData)
	}
	replay := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
	replayValue := artifactWriteJSON(t, replay)
	if replay.Code != http.StatusOK || replayValue["provenance"].(map[string]any)["replay"] != true || replayValue["data"].(map[string]any)["link_id"] != firstData["link_id"] {
		t.Fatalf("link replay changed link identity: status=%d body=%s", replay.Code, replay.Body.String())
	}
	links, err := store.ListArtifactLinks(context.Background(), "write-demo", storage.ArtifactLinkFilter{LinkType: "task_generated_from_spec", SourceID: acceptedVersionID, TargetID: task.ID})
	if err != nil || len(links) != 1 {
		t.Fatalf("expected one stable back link: %+v err=%v", links, err)
	}
	_ = actorID
}

func TestLinkExistingSpecTaskSameKeyDifferentTaskPreservesRustReplaySemantics(t *testing.T) {
	store, handler, artifactID, _, _, _ := acceptedSpecTaskFixture(t, false)
	ctx := context.Background()
	firstTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "write-demo", Title: "First target", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "write-demo", Title: "Second target", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/write-demo/specs/" + artifactID + "/link-task"
	headers := map[string]string{"Idempotency-Key": "same-operation-different-task"}
	post := func(taskID string) *httptest.ResponseRecorder {
		t.Helper()
		return artifactWriteRequest(handler, http.MethodPost, path, map[string]string{"manifest_item_id": "phase-1:T002", "task_id": taskID}, headers)
	}
	first := post(firstTask.ID)
	if first.Code != http.StatusCreated {
		t.Fatalf("first link status=%d body=%s", first.Code, first.Body.String())
	}
	firstData := artifactWriteJSON(t, first)["data"].(map[string]any)
	second := post(secondTask.ID)
	secondValue := artifactWriteJSON(t, second)
	secondData := secondValue["data"].(map[string]any)
	if second.Code != http.StatusOK || secondValue["provenance"].(map[string]any)["replay"] != true || secondData["target_id"] != firstTask.ID || secondData["link_id"] != firstData["link_id"] {
		t.Fatalf("same-key retry should replay original target/link like Rust: status=%d body=%s", second.Code, second.Body.String())
	}
	runs, err := store.WorkflowRuns(ctx, "write-demo", artifactID)
	if err != nil || len(runs) != 1 || !slices.Contains(runs[0].GeneratedTaskIDs, firstTask.ID) || !slices.Contains(runs[0].GeneratedTaskIDs, secondTask.ID) {
		t.Fatalf("Rust-compatible replay workflow outputs changed: runs=%+v err=%v", runs, err)
	}
}

func TestLinkExistingSpecTaskAuthorizationAndSourceErrorParity(t *testing.T) {
	store, _, artifactID, _, _, _ := acceptedSpecTaskFixture(t, false)
	task, err := store.InsertTask(context.Background(), storage.TaskInsert{
		Project: "write-demo", Title: "Authorized target", Reporter: "fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithConfig(store, "fixture-key", HandlerConfig{ArtifactAuthEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/write-demo/specs/" + artifactID + "/link-task"
	body := map[string]string{"manifest_item_id": "phase-1:T001", "task_id": task.ID}
	allScopes := []string{"task.generate_from_spec", "workflow_run.start", "link.write"}
	for i, missing := range allScopes {
		held := make([]string, 0, len(allScopes)-1)
		for _, scope := range allScopes {
			if scope != missing {
				held = append(held, scope)
			}
		}
		headers := map[string]string{
			"Idempotency-Key": "missing-scope-" + strconv.Itoa(i),
			"X-Agent-Project": "write-demo", "X-Agent-Scopes": strings.Join(held, " "),
		}
		response := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), missing) {
			t.Fatalf("missing %s status=%d body=%s", missing, response.Code, response.Body.String())
		}
	}

	validHeaders := func(key string) map[string]string {
		return map[string]string{
			"Idempotency-Key": key, "X-Agent-Project": "write-demo",
			"X-Agent-Scopes": strings.Join(allScopes, " "),
		}
	}
	wrongProject := validHeaders("wrong-project")
	wrongProject["X-Agent-Project"] = "other-project"
	if response := artifactWriteRequest(handler, http.MethodPost, path, body, wrongProject); response.Code != http.StatusForbidden {
		t.Fatalf("mismatched project status=%d body=%s", response.Code, response.Body.String())
	}
	missingKey := validHeaders(" ")
	if response := artifactWriteRequest(handler, http.MethodPost, path, body, missingKey); response.Code != http.StatusBadRequest || artifactWriteJSON(t, response)["error"] != "idempotency_key_required" {
		t.Fatalf("missing idempotency key status=%d body=%s", response.Code, response.Body.String())
	}

	unknownTask := artifactWriteRequest(handler, http.MethodPost, path, map[string]string{"manifest_item_id": "phase-1:T001", "task_id": "missing-task"}, validHeaders("unknown-task"))
	if unknownTask.Code != http.StatusNotFound || artifactWriteJSON(t, unknownTask)["error"] != "task not found" {
		t.Fatalf("unknown scoped task status=%d body=%s", unknownTask.Code, unknownTask.Body.String())
	}
	unknownVersion := artifactWriteRequest(handler, http.MethodPost, path, map[string]string{"version_id": "missing-version", "manifest_item_id": "phase-1:T001", "task_id": task.ID}, validHeaders("unknown-version"))
	if unknownVersion.Code != http.StatusNotFound || artifactWriteJSON(t, unknownVersion)["error"] != "version not found" {
		t.Fatalf("unknown scoped version status=%d body=%s", unknownVersion.Code, unknownVersion.Body.String())
	}
	missingItem := artifactWriteRequest(handler, http.MethodPost, path, map[string]string{"manifest_item_id": "absent-item", "task_id": task.ID}, validHeaders("unknown-item"))
	if missingItem.Code != http.StatusNotFound || artifactWriteJSON(t, missingItem)["error"] != "manifest_item not found" {
		t.Fatalf("unknown manifest item should match Rust 404: status=%d body=%s", missingItem.Code, missingItem.Body.String())
	}
	missingFields := artifactWriteRequest(handler, http.MethodPost, path, map[string]string{"manifest_item_id": "  ", "task_id": "  "}, validHeaders("missing-fields"))
	if missingFields.Code != http.StatusBadRequest || artifactWriteJSON(t, missingFields)["error"] != "manifest_item_id_required" {
		t.Fatalf("missing required item status=%d body=%s", missingFields.Code, missingFields.Body.String())
	}
}

func acceptedSpecTaskFixture(t *testing.T, newerCurrent bool) (*storage.Store, http.Handler, string, string, string, string) {
	t.Helper()
	store, handler := newArtifactWriteHTTPFixture(t, false)
	manifest := specTaskTestManifest("Original substrate", "Existing task")
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/specs", map[string]any{
		"title": "Accepted workflow spec", "manifest": manifest,
	}, nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("spec import status=%d body=%s", response.Code, response.Body.String())
	}
	value := artifactWriteJSON(t, response)
	data := value["data"].(map[string]any)
	artifactID := data["artifact"].(map[string]any)["artifact_id"].(string)
	firstVersionID := data["version"].(map[string]any)["artifact_version_id"].(string)
	actorID := value["provenance"].(map[string]any)["actor"].(map[string]any)["actor_id"].(string)
	ctx := context.Background()
	if _, err := store.AcceptArtifactVersion(ctx, "write-demo", artifactID, firstVersionID, actorID, nil, nil); err != nil {
		t.Fatalf("accept spec source version: %v", err)
	}
	currentVersionID := firstVersionID
	if newerCurrent {
		newerManifest := specTaskTestManifest("T001 Newer", "T002 Newer")
		payload, err := buildSpecManifestPayload(newerManifest, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		created, err := store.CreateArtifactVersion(ctx, "write-demo", artifactID, storage.ArtifactVersionInsert{
			ParentVersionID: &firstVersionID, Body: stringPtrForFixture("new draft"), StructuredPayload: payload,
			SourceFormat: stringPtrForFixture("gateway-spec-directory"), CreatedByActorID: actorID,
		})
		if err != nil {
			t.Fatal(err)
		}
		currentVersionID = created.Record.ArtifactVersionID
		if err := store.SetArtifactVersionPointers(ctx, "write-demo", artifactID, &currentVersionID, &firstVersionID); err != nil {
			t.Fatal(err)
		}
	}
	return store, handler, artifactID, firstVersionID, currentVersionID, actorID
}

func specTaskTestManifest(firstTitle, secondTitle string) json.RawMessage {
	return json.RawMessage(`{"items":[` + specTaskTestManifestItem("phase-1:T001", "T001", firstTitle) + `,` + specTaskTestManifestItem("phase-1:T002", "T002", secondTitle) + `]}`)
}

func specTaskTestManifestItem(itemID, code, title string) string {
	value, _ := json.Marshal(map[string]any{
		"manifest_item_id": itemID, "task_code": code, "team": "backend", "title": title,
		"dependencies": []string{}, "labels": []string{"gateway-features"},
		"touch_surface":       []string{"server/spec_manifest_write.go"},
		"acceptance_criteria": []string{"stable generated work"}, "validation_plan": []string{"go test ./server"},
	})
	return string(value)
}

func taskTestManifestItem(itemID, title string) specManifestItem {
	return specManifestItem{ManifestItemID: itemID, TaskCode: stringPtrForFixture("T001"), Title: title,
		Dependencies: []string{}, Labels: []string{"generated-from-spec"}, TouchSurface: []string{},
		AcceptanceCriteria: []string{}, ValidationPlan: []string{}}
}

func specTaskOriginHeaders(key string) map[string]string {
	return map[string]string{
		"Idempotency-Key": key, "X-Agent-Session-Id": "11111111-1111-4111-8111-111111111111",
		"X-Agent-Instance-Id": "22222222-2222-4222-8222-222222222222", "X-Agent-Provider": "codex", "X-Agent-Os": "macos",
	}
}

func stringPtrForFixture(value string) *string { return &value }
