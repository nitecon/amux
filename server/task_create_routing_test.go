package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
)

const routedProjectRemote = "github.com/acme/example"

func routedCreateFixture(t *testing.T) (*storage.Store, http.Handler, storage.RegisteredTargetIdentity) {
	t.Helper()
	store, handler := taskAPI(t)
	remote := routedProjectRemote
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "routed", ChannelName: "routed", Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	endpoint := "/v1/projects/routed/task-targets"
	if response := routedCreateRequest(handler, http.MethodGet, endpoint, "", nil, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated target directory status=%d", response.Code)
	}
	if response := taskRequest(handler, http.MethodPost, endpoint, `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","canonical_remote":"github.com/acme/example"}`, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("human target registration status=%d body=%s", response.Code, response.Body.String())
	}
	wrong := taskRequest(handler, http.MethodPost, endpoint, `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","canonical_remote":"github.com/elsewhere/repo"}`, apiActor())
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong bound target registration status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	registeredResponse := taskRequest(handler, http.MethodPost, endpoint, `{"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","canonical_remote":"https://github.com/acme/example.git"}`, apiActor())
	if registeredResponse.Code != http.StatusOK {
		t.Fatalf("target registration status=%d body=%s", registeredResponse.Code, registeredResponse.Body.String())
	}
	var registered storage.RegisteredTarget
	if err := json.Unmarshal(registeredResponse.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	response := taskRequest(handler, http.MethodGet, endpoint, "", nil)
	var directory storage.TargetDirectory
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &directory) != nil || len(directory.Targets) != 1 || directory.CanonicalRemote == nil || *directory.CanonicalRemote != routedProjectRemote {
		t.Fatalf("target directory status=%d body=%s", response.Code, response.Body.String())
	}
	return store, handler, registered.Target
}

func TestRoutedTaskCreateHumanSessionUsesBearerAuditAndCanonicalBinding(t *testing.T) {
	store, handler, target := routedCreateFixture(t)
	body := `{"title":"Human routed task","canonical_remote":"https://github.com/acme/example.git","routing":{"mode":"session","target":` + mustJSON(t, target) + `}}`
	created := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", body, nil)
	if created.Code != http.StatusOK {
		t.Fatalf("routed human create: %d %s", created.Code, created.Body.String())
	}
	var task storage.Task
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Reporter != "user" || task.Status != "todo" || task.StartedAt != nil || task.OwnerOrigin == nil || *task.OwnerOrigin != target.Origin || string(task.Routing) != `{"mode":"session","target":{"origin":{"session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","instance_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","provider":"codex","os":"linux"},"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}}` {
		t.Fatalf("routed human task snapshot: %+v, routing=%s", task, task.Routing)
	}
	events, err := store.TaskEvents(context.Background(), storage.EventQuery{TaskID: &task.ID})
	if err != nil || len(events) != 1 || events[0].Origin != nil {
		t.Fatalf("human create audit should be one event with no native origin: %+v err=%v", events, err)
	}
	reassigned := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks/"+task.ID+"/reassign", `{"target":`+mustJSON(t, target)+`}`, apiActor())
	if reassigned.Code != http.StatusOK {
		t.Fatalf("same execution reassign after human create: %d %s", reassigned.Code, reassigned.Body.String())
	}
	destinationRemote := "github.com/acme/destination"
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "destination", ChannelName: "destination", Kind: "repo", CanonicalRemote: &destinationRemote}); err != nil {
		t.Fatal(err)
	}
	foreignRoute := taskRequest(handler, http.MethodPost, "/v1/projects/destination/tasks", `{"title":"Wrong project","canonical_remote":"github.com/acme/destination","routing":{"mode":"session","target":`+mustJSON(t, target)+`}}`, nil)
	if foreignRoute.Code != http.StatusConflict {
		t.Fatalf("cross-project target route status=%d body=%s", foreignRoute.Code, foreignRoute.Body.String())
	}
	beforeTasks, beforeEvents := routedCreateCounts(t, handler, store)
	wrong := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", `{"title":"Wrong repository","canonical_remote":"github.com/elsewhere/repo","routing":{"mode":"session","target":`+mustJSON(t, target)+`}}`, nil)
	if wrong.Code != http.StatusConflict {
		t.Fatalf("canonical mismatch status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	afterTasks, afterEvents := routedCreateCounts(t, handler, store)
	if beforeTasks != afterTasks || beforeEvents != afterEvents {
		t.Fatalf("canonical mismatch partially created task/event: tasks %d->%d events %d->%d", beforeTasks, afterTasks, beforeEvents, afterEvents)
	}
	newGeneration := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	remote := routedProjectRemote
	if _, err := store.RegisterTarget(context.Background(), "routed", storage.TargetRegistration{Origin: target.Origin, Generation: newGeneration, CanonicalRemote: &remote}, time.Now()); err != nil {
		t.Fatal(err)
	}
	stale := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", body, nil)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale target generation status=%d body=%s", stale.Code, stale.Body.String())
	}
	afterStaleTasks, afterStaleEvents := routedCreateCounts(t, handler, store)
	if beforeTasks != afterStaleTasks || beforeEvents != afterStaleEvents {
		t.Fatalf("stale target partially created task/event: tasks %d->%d events %d->%d", beforeTasks, afterStaleTasks, beforeEvents, afterStaleEvents)
	}
	refreshed := target
	refreshed.Generation = newGeneration
	if _, err := store.RegisterTarget(context.Background(), "routed", storage.TargetRegistration{Origin: refreshed.Origin, Generation: refreshed.Generation, CanonicalRemote: &remote}, time.Now().Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	directory := taskRequest(handler, http.MethodGet, "/v1/projects/routed/task-targets", "", nil)
	var targets storage.TargetDirectory
	if directory.Code != http.StatusOK || json.Unmarshal(directory.Body.Bytes(), &targets) != nil || len(targets.Targets) != 0 {
		t.Fatalf("expired target directory status=%d targets=%+v body=%s", directory.Code, targets.Targets, directory.Body.String())
	}
	expiredBody := `{"title":"Expired target","canonical_remote":"github.com/acme/example","routing":{"mode":"session","target":` + mustJSON(t, refreshed) + `}}`
	expired := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", expiredBody, nil)
	if expired.Code != http.StatusConflict {
		t.Fatalf("expired target create status=%d body=%s", expired.Code, expired.Body.String())
	}
	afterExpiredTasks, afterExpiredEvents := routedCreateCounts(t, handler, store)
	if beforeTasks != afterExpiredTasks || beforeEvents != afterExpiredEvents {
		t.Fatalf("expired target partially created task/event: tasks %d->%d events %d->%d", beforeTasks, afterExpiredTasks, beforeEvents, afterExpiredEvents)
	}
}

func TestRoutedTaskCreateNativeAutoKeepsSingleCreationAudit(t *testing.T) {
	store, handler, _ := routedCreateFixture(t)
	body := `{"title":"Native automatic task","root_reason":"user-directed","specification":"Create the requested work","canonical_remote":"github.com/acme/example","routing":{"mode":"auto"}}`
	created := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", body, apiActor())
	if created.Code != http.StatusOK {
		t.Fatalf("native auto create: %d %s", created.Code, created.Body.String())
	}
	var task storage.Task
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.OwnerOrigin == nil || *task.OwnerOrigin != *apiActor() || string(task.Routing) != `{"mode":"auto"}` {
		t.Fatalf("native auto assignment: owner=%+v routing=%s", task.OwnerOrigin, task.Routing)
	}
	events, err := store.TaskEvents(context.Background(), storage.EventQuery{TaskID: &task.ID})
	if err != nil {
		t.Fatalf("read native create audit: %v", err)
	}
	createdEvents := 0
	for _, event := range events {
		if event.Kind != "task_created" {
			continue
		}
		createdEvents++
		if event.Origin == nil || *event.Origin != *apiActor() {
			t.Errorf("native creation audit origin=%+v", event.Origin)
		}
	}
	if createdEvents != 1 {
		t.Fatalf("native create must produce one creation event, events=%+v err=%v", events, err)
	}
}

func TestRoutedTaskCreateNativeModesAndBroadcastHasSingleWinner(t *testing.T) {
	store, handler, _ := routedCreateFixture(t)
	first := apiActor()
	second := *first
	second.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	for _, mode := range []string{"unassigned", "broadcast"} {
		body := `{"title":"` + mode + ` task","root_reason":"user-directed","specification":"Create the requested work","canonical_remote":"github.com/acme/example","routing":{"mode":"` + mode + `"}}`
		created := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", body, first)
		if created.Code != http.StatusOK {
			t.Fatalf("native %s create: %d %s", mode, created.Code, created.Body.String())
		}
		var task storage.Task
		if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		if task.OwnerOrigin != nil || task.OwnerAgentID != nil || string(task.Routing) != `{"mode":"`+mode+`"}` {
			t.Fatalf("%s routing should remain unassigned at creation: owner=%+v routing=%s", mode, task.OwnerOrigin, task.Routing)
		}
		if mode == "broadcast" {
			path := "/v1/projects/routed/tasks/" + task.ID
			claim := taskRequest(handler, http.MethodPatch, path, `{"status":"in_progress"}`, first)
			if claim.Code != http.StatusOK {
				t.Fatalf("broadcast first claim: %d %s", claim.Code, claim.Body.String())
			}
			secondClaim := taskRequest(handler, http.MethodPatch, path, `{"status":"in_progress"}`, &second)
			if secondClaim.Code != http.StatusConflict {
				t.Fatalf("broadcast second claimant status=%d body=%s", secondClaim.Code, secondClaim.Body.String())
			}
		}
	}
	beforeTasks, beforeEvents := routedCreateCounts(t, handler, store)
	bad := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", `{"title":"bad mode","root_reason":"user-directed","specification":"Create the requested work","canonical_remote":"github.com/acme/example","routing":{"mode":"invented"}}`, first)
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported routing status=%d body=%s", bad.Code, bad.Body.String())
	}
	afterTasks, afterEvents := routedCreateCounts(t, handler, store)
	if beforeTasks != afterTasks || beforeEvents != afterEvents {
		t.Fatalf("unsupported route partially created task/event: tasks %d->%d events %d->%d", beforeTasks, afterTasks, beforeEvents, afterEvents)
	}
	beforeTasks, beforeEvents = routedCreateCounts(t, handler, store)
	wrongBinding := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", `{"title":"wrong bound broadcast","routing":{"mode":"broadcast"},"canonical_remote":"github.com/elsewhere/repo"}`, nil)
	if wrongBinding.Code != http.StatusConflict {
		t.Fatalf("broadcast canonical mismatch status=%d body=%s", wrongBinding.Code, wrongBinding.Body.String())
	}
	afterTasks, afterEvents = routedCreateCounts(t, handler, store)
	if beforeTasks != afterTasks || beforeEvents != afterEvents {
		t.Fatalf("broadcast canonical mismatch partially created task/event: tasks %d->%d events %d->%d", beforeTasks, afterTasks, beforeEvents, afterEvents)
	}
}

func TestRoutedTaskCreateRequiresRepoBindingButAllowsUnboundAdhoc(t *testing.T) {
	store, handler, _ := routedCreateFixture(t)
	repoMissing := taskRequest(handler, http.MethodPost, "/v1/projects/routed/tasks", `{"title":"missing repo binding","routing":{"mode":"broadcast"}}`, nil)
	if repoMissing.Code != http.StatusConflict {
		t.Fatalf("repo without canonical binding status=%d body=%s", repoMissing.Code, repoMissing.Body.String())
	}
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "local", ChannelName: "local", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	adhoc := taskRequest(handler, http.MethodPost, "/v1/projects/local/tasks", `{"title":"local broadcast","routing":{"mode":"broadcast"}}`, nil)
	if adhoc.Code != http.StatusOK {
		t.Fatalf("unbound adhoc route status=%d body=%s", adhoc.Code, adhoc.Body.String())
	}
}

func routedCreateCounts(t *testing.T, handler http.Handler, store *storage.Store) (int, int) {
	t.Helper()
	response := taskRequest(handler, http.MethodGet, "/v1/projects/routed/tasks?status=todo,in_progress,user_review,done,cancelled&include_stale=true", "", nil)
	var tasks []storage.TaskSummary
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &tasks) != nil {
		t.Fatalf("list routed tasks: %d %s", response.Code, response.Body.String())
	}
	project := "routed"
	events, err := store.TaskEvents(context.Background(), storage.EventQuery{Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks), len(events)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func routedCreateRequest(handler http.Handler, method, path, body string, actor *storage.AgentOrigin, bearer bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer {
		req.Header.Set("Authorization", "Bearer task-key")
	}
	if actor != nil {
		req.Header.Set("X-Agent-Session-Id", actor.SessionID)
		req.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
		req.Header.Set("X-Agent-Provider", actor.Provider)
		req.Header.Set("X-Agent-OS", actor.OS)
		req.Header.Set("X-Agent-Id", "worker")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}
