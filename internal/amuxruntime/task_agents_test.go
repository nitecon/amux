package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestRunTaskAgentsIsReadOnlyAndReportsRemoteOSAndHost(t *testing.T) {
	checkout := t.TempDir()
	for _, args := range [][]string{{"init", "-q", checkout}, {"-C", checkout, "remote", "add", "origin", "https://github.com/example/project.git"}} {
		command := exec.Command("git", args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(checkout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	now := time.Now().UnixMilli()
	remote := "github.com/example/project"
	target := storage.RegisteredTarget{
		Target: storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{
			SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "windows",
		}, Generation: uuid.NewString()},
		SeenAt: now, ExpiresAt: now + 60000,
		DisplayName: stringPointer("Remote agent"), Hostname: stringPointer("Windows host"),
	}
	var methods []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet || r.Header.Get("X-Agent-Session-Id") != "" || r.Header.Get("X-Agent-Instance-Id") != "" {
			t.Errorf("discovery request was not anonymous and read-only: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/projects":
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: &remote}}})
		case "/v1/projects/demo/task-targets":
			_ = json.NewEncoder(w).Encode(storage.TargetDirectory{CanonicalRemote: &remote, Targets: []storage.RegisteredTarget{target}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"--server", host.URL, "--project", "demo", "agents", "--os", "windows"}
	if err := runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "fixture" }); err != nil {
		t.Fatalf("runTasks: %v stderr=%q", err, stderr.String())
	}
	var got storage.TargetDirectory
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("JSON output %q: %v", stdout.String(), err)
	}
	if got.CanonicalRemote == nil || *got.CanonicalRemote != remote || len(got.Targets) != 1 || got.Targets[0].Target != target.Target || got.Targets[0].Hostname == nil || *got.Targets[0].Hostname != "Windows host" {
		t.Fatalf("discovery output lost binding or remote host metadata: %+v", got)
	}
	if strings.Join(methods, ",") != "GET /v1/projects,GET /v1/projects/demo/task-targets" {
		t.Fatalf("discovery requests=%v", methods)
	}
}

func TestRunTaskAgentsRejectsExtraArgumentsAndInvalidOS(t *testing.T) {
	for _, args := range [][]string{
		{"--server", "http://127.0.0.1:7913", "--project", "demo", "agents", "--os", "plan9"},
		{"--server", "http://127.0.0.1:7913", "--project", "demo", "agents", "unexpected"},
	} {
		var stdout, stderr bytes.Buffer
		err := runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "fixture" })
		if err == nil || stdout.Len() != 0 {
			t.Fatalf("args %v: err=%v stdout=%q", args, err, stdout.String())
		}
	}
}

func stringPointer(value string) *string { return &value }
