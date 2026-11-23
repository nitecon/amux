package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestTaskAgentsChecksCheckoutBindingAndFiltersDirectoryOnce(t *testing.T) {
	now := time.Now().UnixMilli()
	remote := "github.com/example/project"
	makeTarget := func(os string, expires int64, display string) storage.RegisteredTarget {
		return storage.RegisteredTarget{
			Target: storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{
				SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: os,
			}, Generation: uuid.NewString()},
			SeenAt: now - 1000, ExpiresAt: expires,
			DisplayName: &display, Hostname: ptrString("remote host"),
		}
	}
	windows := makeTarget("windows", now+60000, "Windows agent")
	linux := makeTarget("linux", now+60000, "Linux agent")
	expired := makeTarget("windows", now-1, "Expired agent")
	var projectsCalls, targetsCalls int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s %s", r.Method, r.URL)
		}
		switch r.URL.Path {
		case "/v1/projects":
			projectsCalls++
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: &remote}}})
		case "/v1/projects/demo/task-targets":
			targetsCalls++
			_ = json.NewEncoder(w).Encode(storage.TargetDirectory{CanonicalRemote: &remote, Targets: []storage.RegisteredTarget{windows, linux, expired}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	client, err := New(host.URL, "key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.TaskAgents(context.Background(), "demo", "https://github.com/example/project.git", "windows")
	if err != nil {
		t.Fatal(err)
	}
	if projectsCalls != 1 || targetsCalls != 1 || len(got.Targets) != 1 {
		t.Fatalf("requests projects=%d targets=%d; targets=%+v", projectsCalls, targetsCalls, got.Targets)
	}
	if got.Targets[0].Target != windows.Target || got.Targets[0].DisplayName == nil || *got.Targets[0].DisplayName != "Windows agent" || got.Targets[0].Hostname == nil || *got.Targets[0].Hostname != "remote host" {
		t.Fatalf("remote target metadata was not preserved: %+v", got.Targets[0])
	}
}

func TestTaskAgentsRejectsMismatchedDirectoryBinding(t *testing.T) {
	projectRemote, directoryRemote := "github.com/example/project", "github.com/other/project"
	var targetCalls int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects":
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: &projectRemote}}})
		case "/v1/projects/demo/task-targets":
			targetCalls++
			_ = json.NewEncoder(w).Encode(storage.TargetDirectory{CanonicalRemote: &directoryRemote})
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	}))
	defer host.Close()
	client, err := New(host.URL, "key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.TaskAgents(context.Background(), "demo", projectRemote, ""); err == nil {
		t.Fatal("mismatched target directory binding was accepted")
	}
	if targetCalls != 1 {
		t.Fatalf("target directory requests=%d", targetCalls)
	}
}
