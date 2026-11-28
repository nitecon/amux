package worksubmission

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestSubmissionGoalScopeAllowsUntaggedTaskAndRequiresTaggedMatch(t *testing.T) {
	release := "release"
	other := "other"
	for _, test := range []struct {
		name     string
		selected *string
		task     *string
		want     bool
	}{
		{"selected goal may submit untagged task", &release, nil, true},
		{"unscoped task", nil, nil, true},
		{"tagged task requires a selected goal", nil, &release, false},
		{"tagged task requires matching goal", &other, &release, false},
		{"matching tagged task", &release, &release, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := submissionGoalMatches(test.selected, test.task); got != test.want {
				t.Fatalf("submissionGoalMatches = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSubmitWorkPathsRequireOneBoundedJSONArray(t *testing.T) {
	for _, contents := range []string{`["main.go"] ["extra.go"]`, `["main.go"] trailing`, `null`, `{}`, `["main.go", null]`} {
		path := filepath.Join(t.TempDir(), "paths.json")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSubmitWorkPaths(path); err == nil {
			t.Fatalf("accepted invalid path-list document %q", contents)
		}
	}

	path := filepath.Join(t.TempDir(), "paths.json")
	if err := os.WriteFile(path, []byte("[\"main.go\"]  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := readSubmitWorkPaths(path)
	if err != nil || len(paths) != 1 || paths[0] != "main.go" {
		t.Fatalf("valid single JSON array was rejected: %#v %v", paths, err)
	}
	if err := os.WriteFile(path, []byte(`[" main.go ","","-dash.go"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err = readSubmitWorkPaths(path)
	if err != nil || !reflect.DeepEqual(paths, []string{" main.go ", "", "-dash.go"}) {
		t.Fatalf("path manifest did not preserve literal string elements: %#v %v", paths, err)
	}
	if err := os.WriteFile(path, []byte(`["`+strings.Repeat("x", maxSubmitWorkPathsBytes)+`"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSubmitWorkPaths(path); err == nil || !strings.Contains(err.Error(), "4 MiB") {
		t.Fatalf("oversized path-list document was not bounded: %v", err)
	}
}

func TestSubmissionRootKeepsExplicitRepositorySubdirectory(t *testing.T) {
	repo := t.TempDir()
	gitFixture(t, repo, "remote", "https://github.com/team/repo.git")
	requested := filepath.Join(repo, "nested", "checkout")
	if err := os.MkdirAll(requested, 0o700); err != nil {
		t.Fatal(err)
	}
	requested, err := filepath.EvalSymlinks(requested)
	if err != nil {
		t.Fatal(err)
	}
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	root, identity, err := canonicalSubmissionRoot(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if root != requested || identity.Directory != repo {
		t.Fatalf("explicit request root=%q, repository identity root=%q; want %q and %q", root, identity.Directory, requested, repo)
	}
}

func TestAdhocProjectAcceptsLocalBareCheckoutRemote(t *testing.T) {
	repo := t.TempDir()
	gitFixture(t, repo, "remote", filepath.Join(t.TempDir(), "local-bare.git"))
	_, identity, err := canonicalSubmissionRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	adhoc := storage.ProjectOverview{Project: storage.Project{Ident: "adhoc", Kind: "adhoc"}}
	if !CheckoutMatchesProject(adhoc, identity) {
		t.Fatalf("local bare checkout was rejected for an unbound ad-hoc project: %+v", identity)
	}
	if CheckoutMatchesProject(adhoc, projectid.Identity{Upstream: "github.com/team/repo"}) {
		t.Fatal("ad-hoc project accepted a supported upstream even though target registration requires no remote binding")
	}
	repository := storage.ProjectOverview{Project: storage.Project{Ident: "repo", Kind: "repo"}}
	if CheckoutMatchesProject(repository, identity) {
		t.Fatal("checkout with no canonical repository identity matched an unbound repository project")
	}
}

func TestWorkSubmissionOwnerPreflightRunsBeforePathFileRead(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "preflight.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	remote := "github.com/team/demo"
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	owner := agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Owned task", Reporter: "fixture"}, &owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTaskTransition(ctx, "demo", task.ID, "in_progress", &owner, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "submission-test-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "submission-test-key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitFixture(t, repo, "origin", "https://github.com/team/demo.git")
	wrongOwner := owner
	wrongOwner.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	_, err = SubmitTask(ctx, client, Input{
		Project: "demo", Root: repo, TaskID: task.ID, Message: "publish", PathsFile: filepath.Join(t.TempDir(), "must-not-be-read.json"), Origin: &wrongOwner,
	}, filepath.Join(t.TempDir(), "preferences.json"))
	if err == nil || !strings.Contains(err.Error(), "exact execution") || strings.Contains(err.Error(), "must-not-be-read") {
		t.Fatalf("work submission did not reject ownership before reading paths: %v", err)
	}
	current, err := store.Task(ctx, "demo", task.ID)
	if err != nil || current.Task.Status != "in_progress" || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != owner {
		t.Fatalf("preflight changed the owned task: %+v %v", current, err)
	}
	invalidPreferences := filepath.Join(t.TempDir(), "invalid-preferences.json")
	if err := os.WriteFile(invalidPreferences, []byte(`{"work_submission":{"mode":null}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missingPaths := filepath.Join(t.TempDir(), "must-not-be-read.json")
	_, err = SubmitTask(ctx, client, Input{
		Project: "demo", Root: repo, TaskID: task.ID, Message: "publish", PathsFile: missingPaths, Origin: &owner,
	}, invalidPreferences)
	if err == nil || !strings.Contains(err.Error(), "mode cannot be null") || strings.Contains(err.Error(), missingPaths) {
		t.Fatalf("invalid settings were not rejected before path/Git submission: %v", err)
	}
}

func TestWorkSubmissionAllowsAnotherOwnedTaskInBoundConversationProject(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "same-project-task.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	remote := "github.com/team/demo"
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	owner := agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	boundTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Conversation task", Reporter: "fixture"}, &owner)
	if err != nil {
		t.Fatal(err)
	}
	ownedTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Separate owned task", Reporter: "fixture"}, &owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{boundTask.ID, ownedTask.ID} {
		if _, err := store.ApplyTaskTransition(ctx, "demo", task, "in_progress", &owner, storage.TaskTransitionPolicy{}); err != nil {
			t.Fatal(err)
		}
	}
	conversation, err := store.CreateConversation(ctx, "demo", boundTask.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "same-project-task-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "same-project-task-key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitFixture(t, repo, "origin", "https://github.com/team/demo.git")
	missingPaths := filepath.Join(t.TempDir(), "paths.json")
	_, err = SubmitTask(ctx, client, Input{
		Project: "demo", ConversationID: conversation.ID, Root: repo, TaskID: ownedTask.ID,
		Message: "submit owned task", PathsFile: missingPaths, Origin: &owner,
	}, filepath.Join(t.TempDir(), "preferences.json"))
	if err == nil || !strings.Contains(err.Error(), "cannot read path list") || strings.Contains(err.Error(), "bound conversation") {
		t.Fatalf("same-project owned task was rejected by the conversation's different task binding: %v", err)
	}
}

func TestWorkSubmissionPreferencesDefaultAndPreserveExistingRootFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway-preferences.json")
	if err := os.WriteFile(path, []byte(`{"mode":"client","enrolled":{"node_id":"saved-node"},"connection":{"node_id":"existing-connection"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPreferences(path)
	if err != nil || got != DefaultPreferences() {
		t.Fatalf("missing nested work_submission did not use defaults: %+v %v", got, err)
	}
	emptyNested := filepath.Join(filepath.Dir(path), "empty-work-preferences.json")
	if err := os.WriteFile(emptyNested, []byte("{\"work_submission\":{}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if nested, err := LoadPreferences(emptyNested); err != nil || nested != DefaultPreferences() {
		t.Fatalf("missing nested fields did not use defaults: %+v %v", nested, err)
	}
	got.Mode, got.BranchPrefix = PullRequestMode, "work/"
	if err := SavePreferences(path, got); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var enrolled, connection map[string]string
	if json.Unmarshal(root["enrolled"], &enrolled) != nil || json.Unmarshal(root["connection"], &connection) != nil || string(root["mode"]) != `"client"` || enrolled["node_id"] != "saved-node" || connection["node_id"] != "existing-connection" {
		t.Fatalf("preference update removed unrelated root fields: %s", data)
	}
	loaded, err := LoadPreferences(path)
	if err != nil || loaded != got {
		t.Fatalf("saved source-compatible nested preferences = %+v, %v", loaded, err)
	}
}

func TestWorkSubmissionPreferencesRejectNullAndCorruptRootWithoutOverwrite(t *testing.T) {
	for _, contents := range []string{`{"work_submission":null}`, `{"work_submission":{"mode":null}}`, `{"work_submission":{"mode": null}}`, `{"work_submission":{"branch_prefix":null}}`, `[]`, `{bad json`} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPreferences(path); err == nil {
			t.Fatalf("accepted corrupt or null preference document %q", contents)
		}
		if err := SavePreferences(path, DefaultPreferences()); err == nil {
			t.Fatalf("overwrote corrupt preference document %q", contents)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != contents {
			t.Fatalf("corrupt document changed: %q %v", data, err)
		}
	}
	if _, err := LoadPreferences(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatalf("missing file should use defaults: %v", err)
	}
}

func TestSubmissionCommentBoundsLargePathListsAndKeepsReceiptComplete(t *testing.T) {
	ctx := context.Background()
	files := make([]string, 1400)
	for i := range files {
		files[i] = strings.Repeat("x", 80) + ".go"
	}
	outcome := &Outcome{Reference: "commit abc123", Files: files}
	comment := submissionComment(outcome)
	if len(comment) > maxTaskCommentBytes || !strings.Contains(comment, "1400 files submitted") || !strings.Contains(comment, outcome.Reference) {
		t.Fatalf("large submission comment was not bounded: len=%d text=%q", len(comment), comment)
	}
	receipt := Receipt{Reference: outcome.Reference, Files: outcome.Files}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Receipt
	if err := json.Unmarshal(encoded, &decoded); err != nil || len(decoded.Files) != len(files) || decoded.Files[len(files)-1] != files[len(files)-1] {
		t.Fatalf("bounded comment dropped paths from the returned receipt: files=%d err=%v", len(decoded.Files), err)
	}

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "bounded-comment.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	origin := agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Large submission", Reporter: "fixture"}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTaskTransition(ctx, "demo", task.ID, "in_progress", &origin, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "bounded-comment-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "bounded-comment-key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.TaskComment(ctx, "demo", task.ID, comment, &origin)
	if err != nil {
		t.Fatalf("bounded agent result comment was not accepted: %v", err)
	}
	if _, err := validateSubmissionComment(raw, task.ID, comment, origin); err != nil {
		t.Fatalf("bounded agent result comment acknowledgement failed validation: %v", err)
	}
	detail, err := store.Task(ctx, "demo", task.ID)
	if err != nil || len(detail.Comments) != 1 || detail.Comments[0].Content != comment {
		t.Fatalf("bounded result comment was not committed: %+v %v", detail, err)
	}
}

func TestSubmissionRejectsCommittedWrongCommentAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "wrong-ack.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	origin := agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Owned task", Reporter: "fixture"}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTaskTransition(ctx, "demo", task.ID, "in_progress", &origin, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "wrong-ack-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	httpClient := host.Client()
	httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(request)
		if err != nil || !strings.HasSuffix(request.URL.Path, "/comments") {
			return response, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		var posted storage.TaskComment
		if err := json.Unmarshal(body, &posted); err != nil {
			return nil, err
		}
		posted.Content = "wrong acknowledged content"
		body, err = json.Marshal(posted)
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		return response, nil
	})
	client, err := gatewayclient.New(host.URL, "wrong-ack-key", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	content := "Work submitted: commit abc123\n\nFiles:\n- main.go"
	raw, err := client.TaskComment(ctx, "demo", task.ID, content, &origin)
	if err != nil {
		t.Fatalf("server comment write itself failed: %v", err)
	}
	if _, err := validateSubmissionComment(raw, task.ID, content, origin); err == nil {
		t.Fatal("accepted a mismatched committed comment acknowledgement")
	}
	detail, err := store.Task(ctx, "demo", task.ID)
	if err != nil || len(detail.Comments) != 1 || detail.Comments[0].Content != content || detail.Comments[0].Origin == nil || *detail.Comments[0].Origin != origin {
		t.Fatalf("wrong-ack regression did not prove the server committed the exact agent comment: %+v %v", detail, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func gitFixture(t *testing.T, directory, name, remote string) {
	t.Helper()
	commands := [][]string{{"init", "-b", "main", directory}, {"-C", directory, "remote", "add", name, remote}}
	for _, args := range commands {
		command := exec.Command("git", args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
}
