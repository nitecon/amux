package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type submissionEndToEnd struct {
	root, remote, preferences, conversation string
	store                                   *storage.Store
	host                                    *httptest.Server
	client                                  *gatewayclient.Client
	task                                    *storage.Task
	origin                                  storage.AgentOrigin
}

func submissionFixtureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(variable), "GIT_") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	data, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic submission git %v: %v\n%s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func newSubmissionEndToEnd(t *testing.T, wrap func(http.Handler) http.Handler) submissionEndToEnd {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	f := submissionEndToEnd{root: filepath.Join(base, "checkout"), remote: filepath.Join(base, "remote.git"), preferences: filepath.Join(base, "work-preferences.json"), origin: storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}}
	for _, directory := range []string{f.root, f.remote} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	submissionFixtureGit(t, f.remote, "init", "--bare", "--initial-branch=main", "--template=")
	submissionFixtureGit(t, f.root, "init", "--initial-branch=main", "--template=")
	for key, value := range map[string]string{"user.name": "Submission Fixture", "user.email": "fixture@example.invalid", "commit.gpgSign": "false", "protocol.allow": "never", "protocol.file.allow": "always", "core.hooksPath": filepath.Join(base, "empty-hooks"), "core.autocrlf": "false"} {
		submissionFixtureGit(t, f.root, "config", "--local", key, value)
	}
	for _, name := range []string{"a.txt", "foreign.txt"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte("base\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	submissionFixtureGit(t, f.root, "add", "--all")
	submissionFixtureGit(t, f.root, "commit", "-m", "Synthetic base")
	submissionFixtureGit(t, f.root, "remote", "add", "origin", f.remote)
	submissionFixtureGit(t, f.root, "push", "--set-upstream", "origin", "main")
	var err error
	f.store, err = storage.Open(ctx, filepath.Join(base, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	if err = f.store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Submit fixture work", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ApplyTaskTransition(ctx, "demo", f.task.ID, "in_progress", &f.origin, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	conversation, err := f.store.CreateConversation(ctx, "demo", f.task.ID, &f.origin)
	if err != nil {
		t.Fatal(err)
	}
	f.conversation = conversation.ID
	handler, err := server.NewHandler(f.store, "work-e2e-key")
	if err != nil {
		t.Fatal(err)
	}
	api := handler
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/projects/demo/tasks/") && r.URL.Query().Get("comments") != "0" {
			t.Errorf("ownership preflight loaded comment history: %s", r.URL.RequestURI())
		}
		api.ServeHTTP(w, r)
	})
	if wrap != nil {
		handler = wrap(handler)
	}
	f.host = httptest.NewServer(handler)
	t.Cleanup(f.host.Close)
	f.client, err = gatewayclient.New(f.host.URL, "work-e2e-key", f.host.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.root, "a.txt"), []byte("task edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f submissionEndToEnd) actorArgs() []string {
	return []string{"--server", f.host.URL, "--project", "demo", "--session", f.origin.SessionID, "--instance", f.origin.InstanceID, "--provider", f.origin.Provider, "--os", f.origin.OS, "--work-preferences", f.preferences}
}

func submissionFixtureEnv(key string) string {
	if key == "GATEWAY_API_KEY" {
		return "work-e2e-key"
	}
	return ""
}

func assertSubmissionEndToEnd(t *testing.T, f submissionEndToEnd, receipt submitWorkReceipt) {
	t.Helper()
	if receipt.TaskID != f.task.ID || receipt.Reference == "" || receipt.CommentID == "" || len(receipt.Files) != 1 || receipt.Files[0] != "a.txt" {
		t.Fatalf("incomplete submission receipt: %+v", receipt)
	}
	if receipt.Reference != submissionFixtureGit(t, f.remote, "rev-parse", "refs/heads/main") || submissionFixtureGit(t, f.remote, "show", "main:a.txt") != "task edit" {
		t.Fatal("receipt did not identify the published task change")
	}
	if message := submissionFixtureGit(t, f.remote, "log", "-1", "--format=%B", "main"); !strings.Contains(message, "AmuxID: "+f.task.ID) {
		t.Fatal("published commit was attributed to a different task")
	}
	detail, err := f.store.Task(context.Background(), "demo", f.task.ID)
	if err != nil || detail.Task.Status != "user_review" || detail.Task.OwnerOrigin != nil ||
		detail.Task.ReviewAction == nil || !strings.Contains(*detail.Task.ReviewAction, receipt.Reference) ||
		len(detail.Task.Changeset) != 1 || detail.Task.Changeset[0] != "a.txt" {
		t.Fatalf("submission did not preserve the result and park the task for review: %+v %v", detail, err)
	}
	var comment storage.TaskComment
	for _, entry := range detail.Comments {
		if entry.AuthorType == "agent" {
			if comment.ID != "" {
				t.Fatal("submission posted multiple agent comments")
			}
			comment = entry
		}
	}
	if comment.ID != receipt.CommentID || comment.AuthorType != "agent" || comment.Author != f.origin.SessionID || comment.Origin == nil || *comment.Origin != f.origin || !strings.Contains(comment.Content, receipt.Reference) {
		t.Fatalf("wrong submission comment provenance: %+v", comment)
	}
	reviews, err := f.store.TaskReviewChildren(context.Background(), "demo", f.task.ID)
	if err != nil || len(reviews) != 1 || reviews[0].Status != "todo" ||
		reviews[0].Parent == nil || reviews[0].Parent.ID != f.task.ID {
		t.Fatalf("submission did not queue a separate code review under the parent: %+v %v", reviews, err)
	}
}

func TestWorkSubmissionCLIPathFileAndTopLevelAliasPublishAndPostOneReceipt(t *testing.T) {
	for _, alias := range []bool{false, true} {
		name := "tasks"
		if alias {
			name = "top-level"
		}
		t.Run(name, func(t *testing.T) {
			f := newSubmissionEndToEnd(t, nil)
			pathsFile := filepath.Join(t.TempDir(), "paths.json")
			if err := os.WriteFile(pathsFile, []byte(`["a.txt"]`), 0600); err != nil {
				t.Fatal(err)
			}
			args := f.actorArgs()
			if !alias {
				args = append(args, "submit-work")
			}
			args = append(args, "--directory", f.root, "--paths-file", pathsFile, f.task.ID, "Fixture submission")
			var stdout, stderr bytes.Buffer
			var err error
			if alias {
				err = run(context.Background(), append([]string{"submit-work"}, args...), &stdout, &stderr, submissionFixtureEnv)
			} else {
				err = runTasks(context.Background(), args, &stdout, &stderr, submissionFixtureEnv)
			}
			if err != nil {
				t.Fatalf("submission CLI: %v\n%s", err, &stderr)
			}
			var receipt submitWorkReceipt
			decoder := json.NewDecoder(&stdout)
			if err := decoder.Decode(&receipt); err != nil {
				t.Fatal(err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("CLI emitted more than one JSON receipt: %v %v", extra, err)
			}
			assertSubmissionEndToEnd(t, f, receipt)
		})
	}
}

func TestNativeMCPSubmitWorkUsesPinnedCheckoutAndScopedUntaggedTask(t *testing.T) {
	f := newSubmissionEndToEnd(t, nil)
	arguments := map[string]any{"task_id": f.task.ID, "message": "Native MCP submission", "paths": []string{"a.txt"}}
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "submit_work", "arguments": arguments}})
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"submission-fixture\",\"version\":\"1\"}}}\n" +
		"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\",\"params\":{}}\n" + string(call) + "\n"
	args := append(f.actorArgs(), "--conversation", f.conversation, "--directory", f.root, "--goal", "release")
	var stdout, stderr bytes.Buffer
	if err := runMCP(context.Background(), args, io.NopCloser(strings.NewReader(input)), &stdout, &stderr, submissionFixtureEnv); err != nil {
		t.Fatalf("native MCP: %v %s", err, &stderr)
	}
	decoder := json.NewDecoder(&stdout)
	var receipt submitWorkReceipt
	foundReceipt, foundTool := false, false
	for decoder.More() {
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if len(response.Error) > 0 {
			t.Fatalf("native MCP error: %s", response.Error)
		}
		if response.ID == 2 {
			foundTool = bytes.Contains(response.Result, []byte(`"submit_work"`))
		}
		if response.ID == 3 {
			var result mcpstdio.Result
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result.IsError || len(result.Content) != 1 {
				t.Fatalf("local submit_work failed: %+v", result)
			}
			if err := json.Unmarshal([]byte(result.Content[0].Text), &receipt); err != nil {
				t.Fatal(err)
			}
			foundReceipt = true
		}
	}
	if !foundTool || !foundReceipt {
		t.Fatalf("native MCP omitted local tool or receipt: %s", &stdout)
	}
	assertSubmissionEndToEnd(t, f, receipt)
}

func TestWorkSubmissionUnconfirmedCommentRetainsPublishedReferenceWithoutReplay(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "CLI"
		if native {
			name = "MCP"
		}
		t.Run(name, func(t *testing.T) {
			var comments atomic.Int32
			f := newSubmissionEndToEnd(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					isComment := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments")
					isTool := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tools")
					if !isComment && !isTool {
						next.ServeHTTP(w, r)
						return
					}
					comments.Add(1)
					recorder := httptest.NewRecorder()
					next.ServeHTTP(recorder, r)
					if recorder.Code != http.StatusOK {
						t.Errorf("comment did not commit: %d", recorder.Code)
					}
					var saved storage.TaskComment
					if isComment {
						if err := json.Unmarshal(recorder.Body.Bytes(), &saved); err != nil {
							t.Error(err)
							return
						}
						saved.Content = "synthetic-secret-in-malformed-receipt"
						_ = json.NewEncoder(w).Encode(saved)
						return
					}
					var result mcpstdio.Result
					if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || len(result.Content) != 1 {
						t.Errorf("bad tool fixture: %+v %v", result, err)
						return
					}
					if err := json.Unmarshal([]byte(result.Content[0].Text), &saved); err != nil {
						t.Error(err)
						return
					}
					saved.Content = "synthetic-secret-in-malformed-receipt"
					data, _ := json.Marshal(saved)
					result.Content[0].Text = string(data)
					_ = json.NewEncoder(w).Encode(result)
				})
			})
			var failure string
			if native {
				args, _ := json.Marshal(map[string]any{"task_id": f.task.ID, "message": "Native submission", "paths": []string{"a.txt"}})
				result, err := callSubmitWorkMCP(context.Background(), f.client, "demo", f.conversation, f.root, f.origin, nil, f.preferences, args)
				if err != nil || !result.IsError || len(result.Content) != 1 {
					t.Fatalf("unconfirmed native comment reported success: %+v %v", result, err)
				}
				failure = result.Content[0].Text
			} else {
				receipt, err := submitWork(context.Background(), f.client, submitWorkInput{Project: "demo", Root: f.root, TaskID: f.task.ID, Message: "CLI submission", Paths: []string{"a.txt"}, Origin: &f.origin}, f.preferences)
				if err == nil || receipt.Reference == "" {
					t.Fatalf("unconfirmed CLI comment reported success or lost receipt: %+v %v", receipt, err)
				}
				failure = err.Error()
			}
			published := submissionFixtureGit(t, f.remote, "rev-parse", "refs/heads/main")
			if !strings.Contains(failure, published) || strings.Contains(failure, "synthetic-secret-in-malformed-receipt") {
				t.Fatalf("failure lost published reference or leaked response payload: %s", failure)
			}
			if comments.Load() != 1 || submissionFixtureGit(t, f.remote, "rev-list", "--count", "main") != "2" {
				t.Fatal("unconfirmed comment caused a repeated comment or task commit")
			}
			detail, err := f.store.Task(context.Background(), "demo", f.task.ID)
			if err != nil || len(detail.Comments) != 1 || !strings.Contains(detail.Comments[0].Content, published) {
				t.Fatalf("fixture did not commit the authentic result comment: %+v %v", detail, err)
			}
		})
	}
}

func TestNativeMCPSubmissionRetainsExistingOwnedTaskScopeBeyondBoundTask(t *testing.T) {
	f := newSubmissionEndToEnd(t, nil)
	parent := f.task.ID
	owned, err := f.store.InsertTask(context.Background(), storage.TaskInsert{Project: "demo", Title: "Owned same-project work", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ApplyTaskTransition(context.Background(), "demo", owned.ID, "in_progress", &f.origin, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	f.task = owned
	args, _ := json.Marshal(map[string]any{"task_id": owned.ID, "message": "Owned same-project work", "paths": []string{"a.txt"}})
	result, err := callSubmitWorkMCP(context.Background(), f.client, "demo", f.conversation, f.root, f.origin, nil, f.preferences, args)
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("submission narrowed existing same-project exact-owner task-tool scope: %+v %v", result, err)
	}
	var receipt submitWorkReceipt
	if err := json.Unmarshal([]byte(result.Content[0].Text), &receipt); err != nil {
		t.Fatal(err)
	}
	assertSubmissionEndToEnd(t, f, receipt)
	parentDetail, err := f.store.Task(context.Background(), "demo", parent)
	if err != nil || len(parentDetail.Comments) != 0 || parentDetail.Task.Status != "in_progress" {
		t.Fatal("owned task submission retargeted the conversation's other task")
	}
}

func TestLargeSubmissionPublishesAndEntersReview(t *testing.T) {
	f := newSubmissionEndToEnd(t, nil)
	paths := make([]string, 1025)
	for i := range paths {
		paths[i] = fmt.Sprintf("large-%04d.txt", i)
		if err := os.WriteFile(filepath.Join(f.root, paths[i]), []byte("submitted\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := submitWork(context.Background(), f.client, submitWorkInput{
		Project: "demo", Root: f.root, TaskID: f.task.ID, Message: "Large submission", Paths: paths, Origin: &f.origin,
	}, f.preferences)
	if err != nil || len(receipt.Files) != len(paths) || receipt.CommentID == "" {
		t.Fatalf("large submission failed: files=%d comment=%q error=%v", len(receipt.Files), receipt.CommentID, err)
	}
	detail, err := f.store.Task(context.Background(), "demo", f.task.ID)
	if err != nil || detail.Task.Status != "user_review" || detail.Task.TaskType != "software" ||
		detail.Task.ReviewAction == nil || !strings.Contains(*detail.Task.ReviewAction, receipt.Reference) {
		t.Fatalf("large submission did not reach review: %+v %v", detail, err)
	}
	published := submissionFixtureGit(t, f.remote, "diff-tree", "--no-commit-id", "--name-only", "-r", receipt.Reference)
	if len(strings.Split(published, "\n")) != len(paths) {
		t.Fatal("published commit did not retain the full changeset")
	}
	ctx := context.Background()
	if _, err := f.client.CloseTask(ctx, "demo", f.task.ID); err == nil {
		t.Fatal("closed submitted work before its review finished")
	}
	reviews, err := f.store.TaskReviewChildren(ctx, "demo", f.task.ID)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("expected one code review before closing: %+v %v", reviews, err)
	}
	reviewer := &storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	review := reviews[0]
	if _, err := f.store.ApplyTaskTransition(ctx, "demo", review.ID, "in_progress", reviewer, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	reviewConversation, err := f.store.CreateConversation(ctx, "demo", review.ID, reviewer)
	if err != nil {
		t.Fatal(err)
	}
	verdict, _ := json.Marshal(map[string]string{"id": review.ID, "outcome": "approved", "summary": "Checked the published files and accepted the change."})
	response, err := f.client.CallTaskToolWithGoal(ctx, "demo", reviewConversation.ID, *reviewer, nil, "task_finish_review", verdict)
	if err != nil || response.IsError {
		t.Fatalf("review approval failed: %+v %v", response, err)
	}
	detail, err = f.store.Task(ctx, "demo", f.task.ID)
	if err != nil || detail.Task.Status != "done" {
		t.Fatalf("approval did not automatically close the parent: %+v %v", detail, err)
	}
	reviews, err = f.store.TaskReviewChildren(ctx, "demo", f.task.ID)
	if err != nil || len(reviews) != 2 || reviews[0].ID != review.ID {
		t.Fatalf("parent completion should reuse the code review and preserve legacy documentation review: %+v %v", reviews, err)
	}
}

func TestReviewCorrectionsTargetImplementerAndQueueSeparateFinding(t *testing.T) {
	f := newSubmissionEndToEnd(t, nil)
	ctx := context.Background()
	if _, err := submitWork(ctx, f.client, submitWorkInput{
		Project: "demo", Root: f.root, TaskID: f.task.ID, Message: "Review this work", Paths: []string{"a.txt"}, Origin: &f.origin,
	}, f.preferences); err != nil {
		t.Fatal(err)
	}
	reviews, err := f.store.TaskReviewChildren(ctx, "demo", f.task.ID)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("expected a code review: %+v %v", reviews, err)
	}
	reviewer := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	review := reviews[0]
	if _, err := f.store.ApplyTaskTransition(ctx, "demo", review.ID, "in_progress", &reviewer, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	finding := storage.ReviewFinding{Title: "Handle the separate edge case", Details: "Add validation to the separate input path."}
	finish := func() error {
		_, err := f.store.FinishSubmissionReview(ctx, "demo", review.ID, "changes_requested", "Reuse the existing helper instead of duplicating it.", []storage.ReviewFinding{finding}, &reviewer)
		return err
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err == nil {
		t.Fatal("a closed review accepted a second verdict")
	}
	parent, err := f.store.Task(ctx, "demo", f.task.ID)
	if err != nil || parent.Task.Status != "todo" {
		t.Fatalf("correction did not queue the parent: %+v %v", parent, err)
	}
	implementer := f.origin
	implementer.InstanceID = uuid.NewString()
	now := time.Now()
	activity := now.Add(-31 * time.Second).UnixMilli()
	report := storage.PresenceReport{Source: "review-fixture", Sequence: 1, Agents: []storage.PresenceAgent{
		{ProjectIdent: "demo", Origin: reviewer, AutoAssign: true, LastActivityAt: &activity},
	}}
	if _, err := f.store.ReportPresence(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	parent, err = f.store.Task(ctx, "demo", f.task.ID)
	if err != nil || parent.Task.OwnerOrigin != nil {
		t.Fatalf("reviewer received the implementer's corrections: %+v %v", parent, err)
	}
	adopted, err := f.store.AdoptWorkerAssignment(ctx, "demo", &reviewer)
	if err != nil || adopted == nil || adopted.Task.Title != finding.Title || adopted.Task.Parent == nil || adopted.Task.Parent.ID != f.task.ID {
		t.Fatalf("separate finding did not reach the available reviewer: %+v %v", adopted, err)
	}
	report.Sequence++
	if _, err := f.store.InsertTask(ctx, storage.TaskInsert{
		Project: "demo", Title: "Another queued review", Reporter: "fixture", Labels: []string{"task-review"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	report.Agents = append(report.Agents, storage.PresenceAgent{ProjectIdent: "demo", Origin: implementer, AutoAssign: true, LastActivityAt: &activity})
	if _, err := f.store.ReportPresence(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	parent, err = f.store.Task(ctx, "demo", f.task.ID)
	if err != nil || parent.Task.OwnerOrigin == nil || *parent.Task.OwnerOrigin != implementer {
		t.Fatalf("correction did not return to the implementer's current execution: %+v %v", parent, err)
	}
}

func TestNativeMCPLargeSubmitWorkListReachesTaskValidationWithoutArgvExpansion(t *testing.T) {
	var gatewayCalls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls.Add(1)
		http.Error(w, "must not contact gateway for invalid task", 500)
	}))
	defer host.Close()
	paths := make([]string, 16673)
	for i := range paths {
		paths[i] = fmt.Sprintf("assets/actor_%05d_%s.scene", i, strings.Repeat("a", 80))
	}
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "submit_work", "arguments": map[string]any{"task_id": "not-a-uuid", "message": "Move assets", "paths": paths, "resume_commit": "abc123"}}})
	if len(request) <= 1<<20 || len(request) >= 4<<20 {
		t.Fatalf("source-sized request no longer exercises bounded large input: %d", len(request))
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"large-submission-fixture","version":"1"}}}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + string(request) + "\n"
	var stdout, stderr bytes.Buffer
	args := []string{"--server", host.URL, "--project", "demo", "--conversation", uuid.NewString(), "--session", uuid.NewString(), "--instance", uuid.NewString(), "--provider", "codex", "--os", "linux", "--directory", t.TempDir(), "--work-preferences", filepath.Join(t.TempDir(), "settings.json")}
	if err := runMCP(context.Background(), args, io.NopCloser(strings.NewReader(input)), &stdout, &stderr, submissionFixtureEnv); err != nil {
		t.Fatalf("source-sized MCP request did not reach local task validation: %v", err)
	}
	decoder := json.NewDecoder(&stdout)
	found := false
	for {
		var response struct {
			ID     int             `json:"id"`
			Result mcpstdio.Result `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		err := decoder.Decode(&response)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if response.ID != 2 {
			continue
		}
		found = true
		if len(response.Error) != 0 {
			t.Fatalf("large request failed at the RPC boundary: %s", response.Error)
		}
		if !response.Result.IsError || len(response.Result.Content) != 1 || !strings.Contains(strings.ToLower(response.Result.Content[0].Text), "uuid") {
			t.Fatalf("large request failed before task ID validation: %+v", response.Result)
		}
	}
	if !found || gatewayCalls.Load() != 0 {
		t.Fatalf("large invalid task request was dropped or reached gateway: found=%v calls=%d", found, gatewayCalls.Load())
	}
}

func TestNativeReleaseAndSubmitRejectEveryUnconfirmedOwnershipShape(t *testing.T) {
	for _, operation := range []string{"release", "submit-work"} {
		for _, caseName := range []string{"unclaimed", "already done", "legacy claim without origin", "another session", "another instance", "partial origin", "wrong project"} {
			t.Run(operation+"/"+caseName, func(t *testing.T) {
				var writes atomic.Int32
				f := newSubmissionEndToEnd(t, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodGet {
							writes.Add(1)
							http.Error(w, "mutation must not be attempted", 500)
							return
						}
						if !strings.HasPrefix(r.URL.Path, "/v1/projects/demo/tasks/") {
							next.ServeHTTP(w, r)
							return
						}
						recorder := httptest.NewRecorder()
						next.ServeHTTP(recorder, r)
						var task map[string]any
						if err := json.Unmarshal(recorder.Body.Bytes(), &task); err != nil {
							t.Error(err)
							return
						}
						switch caseName {
						case "unclaimed":
							task["status"] = "todo"
							task["owner_origin"] = nil
						case "already done":
							task["status"] = "done"
						case "legacy claim without origin":
							task["owner_origin"] = nil
						case "another session":
							task["owner_origin"].(map[string]any)["session_id"] = uuid.NewString()
						case "another instance":
							task["owner_origin"].(map[string]any)["instance_id"] = uuid.NewString()
						case "partial origin":
							task["owner_origin"] = map[string]any{"session_id": task["owner_origin"].(map[string]any)["session_id"]}
						case "wrong project":
							task["project_ident"] = "other"
						}
						_ = json.NewEncoder(w).Encode(task)
					})
				})
				before, remote := submissionFixtureGit(t, f.root, "status", "--porcelain=v1"), submissionFixtureGit(t, f.remote, "rev-parse", "refs/heads/main")
				args := append(f.actorArgs(), operation)
				if operation == "submit-work" {
					args = append(args, "--directory", f.root, "--paths-file", filepath.Join(t.TempDir(), "unread-path-list.json"), f.task.ID, "Fixture submission")
				} else {
					args = append(args, f.task.ID)
				}
				var stdout, stderr bytes.Buffer
				err := runTasks(context.Background(), args, &stdout, &stderr, submissionFixtureEnv)
				if err == nil || writes.Load() != 0 || strings.Contains(err.Error(), "unread-path-list") {
					t.Fatalf("unconfirmed ownership reached paths or mutation: %v writes=%d", err, writes.Load())
				}
				if operation == "submit-work" && !strings.Contains(err.Error(), "work was not submitted") {
					t.Fatalf("preflight failure did not state that work was not submitted: %v", err)
				}
				if submissionFixtureGit(t, f.root, "status", "--porcelain=v1") != before || submissionFixtureGit(t, f.root, "rev-parse", "HEAD") != remote || submissionFixtureGit(t, f.remote, "rev-parse", "refs/heads/main") != remote {
					t.Fatal("ownership refusal changed checkout or publication")
				}
			})
		}
	}
}
