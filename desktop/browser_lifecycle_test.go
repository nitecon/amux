package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func TestConversationWindowRefreshIncludesIDsInsertedWithinLoadedRange(t *testing.T) {
	ids := make([]string, 150)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%03d", i)
	}
	var inserted bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/conversations") {
			http.NotFound(w, r)
			return
		}
		current := append([]string(nil), ids...)
		if inserted {
			current = append(current, "id-050a")
		}
		sort.Strings(current)
		after := r.URL.Query().Get("after_id")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page := make([]storage.Conversation, 0, limit)
		for _, id := range current {
			if id <= after {
				continue
			}
			page = append(page, storage.Conversation{ID: id, ProjectIdent: "demo", TaskID: "task", Provider: "codex"})
			if len(page) == limit {
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "browser-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	first, _, more, err := loadConversationWindow(context.Background(), client, "demo", "task", 150)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 150 || !more {
		t.Fatalf("initial loaded window len=%d more=%t", len(first), more)
	}
	inserted = true
	refreshed, _, more, err := loadConversationWindow(context.Background(), client, "demo", "task", len(first))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range refreshed {
		if item.ID == "id-050a" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("refresh skipped a conversation inserted inside the previously loaded ID range")
	}
	if len(refreshed) != 150 || !more {
		t.Fatalf("refreshed loaded window len=%d more=%t", len(refreshed), more)
	}
}

func TestBrowserFencesOutOfOrderProjectTaskResponses(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	alphaStarted, releaseAlpha := make(chan struct{}, 1), make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/projects":
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "alpha"}}, {Project: storage.Project{Ident: "beta"}}})
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			if strings.Contains(r.URL.Path, "/alpha/") {
				select {
				case alphaStarted <- struct{}{}:
				default:
				}
				<-releaseAlpha // Return after the newer project response to exercise the generation fence.
				_ = json.NewEncoder(w).Encode([]storage.TaskSummary{{ID: "alpha-task", Title: "Alpha task", Status: "todo"}})
				return
			}
			_ = json.NewEncoder(w).Encode([]storage.TaskSummary{{ID: "beta-task", Title: "Beta task", Status: "todo"}})
		case strings.HasSuffix(r.URL.Path, "/conversations"):
			_ = json.NewEncoder(w).Encode([]storage.Conversation{})
		case strings.Contains(r.URL.Path, "/tasks/"):
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "project_ident": "beta", "title": "Beta task", "status": "todo", "specification": "", "comments": []storage.TaskComment{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "browser-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Project: "beta", Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	browser.Start(context.Background())
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedProject == "beta" && len(browser.taskItems) == 1 && browser.taskItems[0].ID == "beta-task"
	})
	callbacks.dispatch(func() { browser.projects.Select(0) })
	deadline := time.Now().Add(3 * time.Second)
	for {
		callbacks.pump()
		select {
		case <-alphaStarted:
			goto alphaLoading
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("alpha task request did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
alphaLoading:
	callbacks.dispatch(func() { browser.projects.Select(1) })
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedProject == "beta" && len(browser.taskItems) == 1 && browser.taskItems[0].ID == "beta-task"
	})
	close(releaseAlpha)
	time.Sleep(30 * time.Millisecond)
	callbacks.pump()
	if browser.selectedProject != "beta" || len(browser.taskItems) != 1 || browser.taskItems[0].ID != "beta-task" {
		t.Fatal("late alpha response retargeted beta task results")
	}
	browser.Close()
	browser.Wait()
	callbacks.pump()
}

func TestBrowserCloseCancelsSlowLoadsAndWaitDrains(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	started, canceled := make(chan struct{}, 1), make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/projects" {
			started <- struct{}{}
			<-r.Context().Done()
			close(canceled)
			return
		}
		http.NotFound(w, r)
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "browser-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	browser.Start(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for {
		callbacks.pump()
		select {
		case <-started:
			goto requestStarted
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("slow project request did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
requestStarted:
	browser.Close()
	drained := make(chan struct{})
	go func() { browser.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("browser Wait did not drain canceled request and refresh loop")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel the in-flight project request")
	}
	callbacks.pump()
}
