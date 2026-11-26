package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func TestTaskComposerRejectsOversizeDraftsBeforeGatewayRequest(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	var creates, comments atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/demo/tasks":
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","project_ident":"demo","title":"Valid task","status":"todo"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/tasks":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			comments.Add(1)
			http.Error(w, "unexpected comment request", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(host.Close)
	client, err := gatewayclient.New(host.URL, "composer-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait() })
	composer := browser.taskComposer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	browser.mu.Lock()
	browser.ctx, browser.cancel, browser.selectedProject, browser.projectGen = ctx, cancel, "demo", 1
	browser.projectItems = []storage.ProjectOverview{{Project: storage.Project{Ident: "demo", Kind: "adhoc"}}}
	browser.mu.Unlock()
	composer.selectProject("demo")
	lastValidTitle := strings.Repeat("é", 511) + "ab" // Exactly 1024 UTF-8 bytes.
	composer.createTitle.SetText(lastValidTitle)
	if composer.createDraft("demo").title != lastValidTitle {
		t.Fatal("title at the 1024-byte boundary was rejected")
	}
	composer.createTitle.SetText(lastValidTitle + "x") // 1025 UTF-8 bytes.
	if composer.createTitle.Text != lastValidTitle || composer.createDraft("demo").title != lastValidTitle || composer.createStatus.Text != "Task title is too long" || creates.Load() != 0 {
		t.Fatalf("oversized Unicode title was not restored before submit: text=%q draft=%q status=%q posts=%d", composer.createTitle.Text, composer.createDraft("demo").title, composer.createStatus.Text, creates.Load())
	}
	composer.createTitle.SetText("Valid title")
	composer.submitCreate()
	waitBrowser(t, callbacks, func() bool { return creates.Load() == 1 })
	waitBrowser(t, callbacks, func() bool { return composer.createDraft("demo").state == taskCreateAccepted })
	if composer.createDraft("demo").createdTaskID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatal("accepted task receipt did not retain its ID")
	}
	if composer.createTitle.Text != "" || composer.createDraft("demo").title != "" || !composer.createdNotice.Visible() || composer.createSubmit.Text != "Submit a new task" {
		t.Fatalf("accepted create did not clear inputs and offer a new task: title=%q notice=%v button=%q", composer.createTitle.Text, composer.createdNotice.Visible(), composer.createSubmit.Text)
	}
	// Reopening the drawer and typing an oversized title must not replace the
	// submission confirmation while the receipt is still accepted.
	composer.createTitle.SetText(strings.Repeat("x", 1025))
	if composer.createdLabel.Text != "Task aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa submitted." || !composer.createdNotice.Visible() {
		t.Fatalf("draft validation replaced the submitted notice: %q", composer.createdLabel.Text)
	}
	fynetest.Tap(composer.submitAnother)
	if composer.createdNotice.Visible() {
		t.Fatal("Submit a new task did not dismiss the submitted notice")
	}
	if !browser.newTaskAccordion.Visible() || !browser.newTaskAccordion.Items[0].Open {
		t.Fatal("Submit a new task did not reopen the drawer")
	}
	if creates.Load() != 1 {
		t.Fatalf("Submit a new task sent an HTTP POST; create count %d", creates.Load())
	}
	composer.newTaskRequest()
	if composer.createDraft("demo").state != taskCreateReady || composer.createTitle.Text != "" {
		t.Fatal("explicit New task request did not clear the completed request")
	}
	composer.createTitle.SetText("A valid title")
	composer.createSpec.SetText(strings.Repeat("é", 32769)) // 65,538 UTF-8 bytes.
	composer.submitCreate()
	if creates.Load() != 1 {
		t.Fatalf("oversized task context sent an HTTP POST; create count %d", creates.Load())
	}
	if composer.createDraft("demo").specification == "" || !strings.Contains(composer.createStatus.Text, "65536 bytes") {
		t.Fatal("oversized task context was not retained with a local validation message")
	}
	if !composer.createSubmit.Disabled() {
		t.Fatal("oversized task context left Create enabled")
	}

	browser.mu.Lock()
	browser.selectedTask = "task-id"
	browser.taskGen = 1
	browser.mu.Unlock()
	composer.selectTask("demo", "task-id")
	composer.commentEntry.SetText(strings.Repeat("c", 65537))
	composer.submitComment()
	if comments.Load() != 0 || !strings.Contains(composer.commentStatus.Text, "65536 bytes") {
		t.Fatal("oversized comment was not rejected locally")
	}
	if !composer.commentSubmit.Disabled() {
		t.Fatal("oversized comment left Post enabled")
	}
}

func TestTaskComposerOffersAllRoutesAndSubmitsFrozenExecution(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	const remote = "github.com/example/project"
	const taskID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	target := storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{SessionID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", InstanceID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Provider: "claude", OS: "linux"}, Generation: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}
	now := time.Now().UnixMilli()
	directory := storage.TargetDirectory{CanonicalRemote: stringPtr(remote), Targets: []storage.RegisteredTarget{{Target: target, SeenAt: now, ExpiresAt: now + 120_000}}}
	var posts atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/task-targets":
			_ = json.NewEncoder(w).Encode(directory)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", Kind: "repo", CanonicalRemote: stringPtr(remote)}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/demo/tasks":
			posts.Add(1)
			var request struct {
				Title           string                     `json:"title"`
				CanonicalRemote *string                    `json:"canonical_remote"`
				Routing         *gatewayclient.TaskRouting `json:"routing"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode routed task create: %v", err)
				return
			}
			if request.CanonicalRemote == nil || *request.CanonicalRemote != remote {
				t.Errorf("create request did not retain the current project binding: %+v", request)
			}
			if request.Title == "Pinned task" && (request.Routing == nil || request.Routing.Mode != gatewayclient.TaskRoutingSession || request.Routing.Target == nil || *request.Routing.Target != target) {
				t.Errorf("session create did not freeze the exact generation: %+v", request)
			}
			if request.Title == "Automatic task" && request.Routing != nil {
				t.Errorf("automatic create should use the server default route: %+v", request)
			}
			ackRouting := gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingAuto}
			var owner *storage.AgentOrigin
			if request.Title == "Pinned task" {
				ackRouting = gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingSession, Target: &target}
				owner = &target.Origin
			}
			routing, _ := json.Marshal(ackRouting)
			_ = json.NewEncoder(w).Encode(storage.Task{ID: taskID, ProjectIdent: "demo", Title: request.Title, Status: "todo", OwnerOrigin: owner, Routing: routing})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(host.Close)
	client, err := gatewayclient.New(host.URL, "composer-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	browser.mu.Lock()
	browser.ctx, browser.cancel = ctx, cancel
	browser.selectedProject, browser.projectGen = "demo", 1
	browser.prefs.Selections[workspaceSelectionKey(browser.prefKey, browser.activeWorkspaceID)] = NavigationSelection{Project: "demo", Status: "all"}
	browser.projectItems = []storage.ProjectOverview{{Project: storage.Project{Ident: "demo", Kind: "repo", CanonicalRemote: stringPtr(remote)}}}
	browser.mu.Unlock()
	c := browser.taskComposer
	c.selectProject("demo")
	waitBrowser(t, callbacks, func() bool { return c.createDraft("demo").targetsLoaded })
	draft := c.createDraft("demo")
	if draft.routing.Mode != gatewayclient.TaskRoutingAuto || c.routingSelect.Selected != "Automatic project routing" {
		t.Fatalf("new project route did not default to automatic: %+v selected=%q", draft.routing, c.routingSelect.Selected)
	}
	for _, label := range []string{"Queue unassigned", "Broadcast to workspace agents"} {
		if _, found := c.routingChoices[label]; !found {
			t.Fatalf("missing route choice %q: %+v", label, c.routingSelect.Options)
		}
	}
	targetLabel := ""
	for label, route := range c.routingChoices {
		if route.Mode == gatewayclient.TaskRoutingSession && route.Target != nil && *route.Target == target {
			targetLabel = label
		}
	}
	if targetLabel == "" {
		t.Fatalf("exact registered execution missing from choices: %+v", c.routingSelect.Options)
	}
	c.routingSelect.SetSelected(targetLabel)
	c.createTitle.SetText("Pinned task")
	c.submitCreate()
	waitBrowser(t, callbacks, func() bool { return draft.state == taskCreateAccepted })
	if posts.Load() != 1 || draft.createdTaskID != taskID || draft.routing.Target == nil || *draft.routing.Target != target {
		t.Fatalf("submission count=%d draft=%+v", posts.Load(), draft)
	}
	c.newTaskRequest()
	c.selectRouting("Automatic project routing")
	c.createTitle.SetText("Automatic task")
	c.submitCreate()
	waitBrowser(t, callbacks, func() bool { return draft.state == taskCreateAccepted && posts.Load() == 2 })
}

func TestTaskComposerFencesDelayedTargetReadAfterProjectChange(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	callbacks := newUICallbacks()
	client, err := gatewayclient.New("http://127.0.0.1:1", "composer-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait() })
	demoRemote, betaRemote := stringPtr("github.com/example/demo"), stringPtr("github.com/example/beta")
	browser.mu.Lock()
	browser.selectedProject, browser.projectGen = "beta", 2
	browser.projectItems = []storage.ProjectOverview{
		{Project: storage.Project{Ident: "demo", Kind: "repo", CanonicalRemote: demoRemote}},
		{Project: storage.Project{Ident: "beta", Kind: "repo", CanonicalRemote: betaRemote}},
	}
	browser.mu.Unlock()
	old := browser.taskComposer.createDraft("demo")
	old.targetsReadGeneration, old.targetsLoading = 1, true
	target := storage.RegisteredTarget{Target: storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}, Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}, SeenAt: time.Now().UnixMilli(), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	browser.taskComposer.finishRoutingTargets(browser.activeWorkspaceID, "demo", 0, 1, demoRemote, []storage.RegisteredTarget{target}, nil)
	if old.targetsLoaded || len(old.targets) != 0 || !old.targetsLoading || old.targetsStatus != "" {
		t.Fatalf("late recipient read was not fenced: %+v", old)
	}
	if browser.taskComposer.createDraft("beta").targetsLoaded {
		t.Fatal("old project recipient read populated the newly selected project")
	}
}

func TestTaskComposerRoutingUnavailableSelectionAndCallbackFence(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	callbacks := newUICallbacks()
	client, err := gatewayclient.New("http://127.0.0.1:1", "composer-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait() })
	remote := stringPtr("github.com/example/demo")
	browser.mu.Lock()
	browser.selectedProject, browser.projectGen = "demo", 11
	browser.projectItems = []storage.ProjectOverview{{Project: storage.Project{Ident: "demo", Kind: "repo", CanonicalRemote: remote}}, {Project: storage.Project{Ident: "beta", Kind: "repo", CanonicalRemote: stringPtr("github.com/example/beta")}}}
	browser.mu.Unlock()
	c := browser.taskComposer
	c.activeProject = "demo"
	now := time.Now()
	oldTarget := storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}, Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}
	replacement := oldTarget
	replacement.Generation = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	draft := c.createDraft("demo")
	draft.title = "Routing parity"
	draft.targetsLoaded = true
	draft.targetsCheckedAt = now
	draft.targets = []storage.RegisteredTarget{{Target: replacement, SeenAt: now.UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli()}}
	draft.routing = gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingSession, Target: &oldTarget}
	c.createTitle.SetText(draft.title)
	c.refreshWidgets()

	t.Run("unavailable generation stays selected", func(t *testing.T) {
		if draft.routing.Mode != gatewayclient.TaskRoutingSession || draft.routing.Target == nil || *draft.routing.Target != oldTarget {
			t.Fatalf("unavailable target was silently changed: %+v", draft.routing)
		}
		if !c.createSubmit.Disabled() || !strings.Contains(c.routingStatus.Text, "no longer live") {
			t.Fatalf("unavailable execution did not block submit: disabled=%v status=%q", c.createSubmit.Disabled(), c.routingStatus.Text)
		}
	})

	t.Run("stale broadcast stays explicit while auto remains available", func(t *testing.T) {
		draft.routing = gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingBroadcast}
		draft.targetsCheckedAt = now.Add(-taskRoutingFreshness - time.Second)
		c.refreshWidgets()
		if draft.routing.Mode != gatewayclient.TaskRoutingBroadcast || !c.createSubmit.Disabled() {
			t.Fatalf("stale broadcast became submit-ready or changed route: route=%+v disabled=%v", draft.routing, c.createSubmit.Disabled())
		}
		draft.routing = gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingAuto}
		c.refreshWidgets()
		if !c.routingAvailable(draft, time.Now()) || c.createSubmit.Disabled() {
			t.Fatalf("automatic route was blocked by stale recipients: status=%q", c.routingStatus.Text)
		}
	})

	t.Run("old project callback cannot populate selected project", func(t *testing.T) {
		draft.targetsReadGeneration, draft.targetsLoading = 7, true
		browser.mu.Lock()
		browser.selectedProject, browser.projectGen = "beta", 12
		browser.mu.Unlock()
		c.activeProject = "beta"
		c.finishRoutingTargets(browser.activeWorkspaceID, "demo", 0, 7, remote, []storage.RegisteredTarget{{Target: replacement, SeenAt: now.UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli()}}, nil)
		if len(c.createDraft("beta").targets) != 0 || c.createDraft("beta").targetsLoaded {
			t.Fatal("delayed demo callback populated beta recipient state")
		}
		if !draft.targetsLoaded || !draft.targetsLoading || len(draft.targets) != 1 || draft.targets[0].Target != replacement || draft.targetsStatus != "" {
			t.Fatalf("stale callback was not fenced from demo's old generation: %+v", draft)
		}
	})
}

func TestTaskComposerPreparesReleaseWithoutReplacingDraftOrReceipt(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	browser := &Browser{prefKey: "release-test", activeWorkspaceID: "workspace-1", selectedProject: "demo", workspaceIDs: []string{"workspace-1"}}
	composer := newTaskComposer(browser)
	composer.buildNewTaskAccordion()
	composer.buildCommentComposer()
	composer.selectProject("demo")
	draft := composer.createDraft("demo")

	t.Run("empty draft prepares the exact editable request", func(t *testing.T) {
		if composer.releasePrepare.Disabled() {
			t.Fatal("New Release should be enabled for an empty composer")
		}
		fynetest.Tap(composer.releasePrepare)
		if draft.title != releaseTaskTitle || draft.specification != releaseTaskSpecification || draft.status != releaseTaskStatus {
			t.Fatalf("release draft title=%q specification=%q status=%q", draft.title, draft.specification, draft.status)
		}
		if draft.state != taskCreateReady || draft.inFlight || draft.routing.Mode != gatewayclient.TaskRoutingAuto || len(composer.workspaceReceipts) != 0 {
			t.Fatalf("preparation changed submission state: %+v receipts=%v", draft, composer.workspaceReceipts)
		}
		if composer.createTitle.Text != releaseTaskTitle || composer.createSpec.Text != releaseTaskSpecification {
			t.Fatal("release request was not placed into the editable composer")
		}
	})

	t.Run("repeat preparation preserves appended notes", func(t *testing.T) {
		notes := "Wait for ticket ABC-123 before releasing."
		context := releaseTaskSpecification + notes
		composer.createSpec.SetText(context)
		composer.prepareReleaseTask()
		if draft.specification != context || composer.createSpec.Text != context {
			t.Fatalf("release preparation replaced appended notes: %q", draft.specification)
		}
	})

	t.Run("title context and goal edits each guard the template", func(t *testing.T) {
		for _, field := range []string{"title", "context", "goal"} {
			t.Run(field, func(t *testing.T) {
				draft.title, draft.specification, draft.goal = "", "", ""
				composer.updating = true
				composer.createTitle.SetText("")
				composer.createSpec.SetText("")
				composer.createGoal.SetText("")
				composer.updating = false
				kept := "Keep this " + field
				switch field {
				case "title":
					draft.title = kept
					composer.createTitle.SetText(kept)
				case "context":
					draft.specification = kept
					composer.createSpec.SetText(kept)
				case "goal":
					draft.goal = kept
					composer.createGoal.SetText(kept)
				}
				composer.refreshWidgets()
				if !composer.releasePrepare.Disabled() {
					t.Fatal("New Release remained enabled over an existing draft")
				}
				composer.prepareReleaseTask()
				switch field {
				case "title":
					if draft.title != kept {
						t.Fatalf("release preparation replaced title: %+v", draft)
					}
				case "context":
					if draft.specification != kept {
						t.Fatalf("release preparation replaced context: %+v", draft)
					}
				case "goal":
					if draft.goal != kept {
						t.Fatalf("release preparation replaced goal: %+v", draft)
					}
				}
			})
		}
	})

	t.Run("any workspace receipt guards preparation", func(t *testing.T) {
		for _, state := range []taskCreateState{taskCreatePending, taskCreateAccepted, taskCreateUnknown} {
			draft.title, draft.specification, draft.goal, draft.state = "", "", "", taskCreateReady
			composer.updating = true
			composer.createTitle.SetText("")
			composer.createSpec.SetText("")
			composer.createGoal.SetText("")
			composer.updating = false
			workspaceID := composer.workspaceID()
			receipt := composer.createDraftFor(workspaceID, "other")
			receipt.state = state
			composer.workspaceReceipts[workspaceID] = workspaceID + "\x00other"
			composer.refreshWidgets()
			if !composer.releasePrepare.Disabled() {
				t.Fatalf("New Release enabled with workspace receipt state %d", state)
			}
			composer.prepareReleaseTask()
			if draft.title != "" || draft.specification != "" || draft.state != taskCreateReady {
				t.Fatalf("receipt state %d changed active project draft: %+v", state, draft)
			}
			delete(composer.workspaceReceipts, workspaceID)
			receipt.state = taskCreateReady
		}
	})
}
