//go:build desktop

package desktopapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/desktop"
	"github.com/nitecon/amux/internal/clientcontrol"
	"github.com/nitecon/amux/server"
)

func TestStopClientForUpdateWaitsForProfileRelease(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "config.yml")
	lock, err := server.LockProfile(settings)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requested := make(chan struct{})
	control := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/shutdown" || r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "unexpected shutdown request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		close(requested)
	})}
	go control.Serve(listener)
	defer control.Close()
	if err := os.WriteFile(settings+".control", []byte(listener.Addr().String()+"\ntest-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- stopClientForUpdate(ctx, settings) }()
	select {
	case <-requested:
	case err := <-done:
		t.Fatalf("returned before requesting shutdown: %v", err)
	case <-ctx.Done():
		t.Fatal("did not request shutdown")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before profile lock was released: %v", err)
	default:
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRefusedUpdateLeavesLocalClientRunning(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "config.yml")
	lock, err := server.LockProfile(settings)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// The running client holds the profile and has no control endpoint here, so
	// any attempt to stop it would report "stop local client".
	err = scheduleDesktopUpdate(t.Context(), filepath.Join(t.TempDir(), "amux"), settings, "https://gateway.test", "", nil)
	if err == nil || !strings.Contains(err.Error(), "schedule update") || strings.Contains(err.Error(), "local client") {
		t.Fatalf("refused update error = %v", err)
	}
}

func TestAttachReusesRunningClient(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "config.yml")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const endpoint = "https://gateway.test"
	cli := filepath.Join(t.TempDir(), "amux")
	if err := os.WriteFile(cli, []byte("companion build"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := clientcontrol.ExecutableDigest(cli)
	if err != nil {
		t.Fatal(err)
	}
	var outdated atomic.Bool
	var shutdowns atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			if !outdated.Load() {
				w.Header().Set(clientcontrol.BuildHeader, digest)
			}
			fmt.Fprintln(w, endpoint)
		case r.Method == http.MethodPost && r.URL.Path == "/shutdown":
			shutdowns.Add(1)
			http.Error(w, "test shutdown refused", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	if err := os.WriteFile(settings+".control", []byte(listener.Addr().String()+"\ntest-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := startOrAttachClient(ctx, cli, settings, endpoint); err != nil {
		t.Fatal(err)
	}
	outdated.Store(true)
	if err := startOrAttachClient(ctx, cli, settings, "https://other.test"); err == nil || shutdowns.Load() != 0 {
		t.Fatal("attempted to restart a client attached to a different gateway")
	}
	if err := startOrAttachClient(ctx, cli, settings, endpoint); err == nil || !strings.Contains(err.Error(), "test shutdown refused") || shutdowns.Load() != 1 {
		t.Fatalf("outdated client restart returned %v with %d shutdown calls", err, shutdowns.Load())
	}
}

func TestMigrateClientConfigPreservesJournalAndExistingYAML(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "config.yml")
	enrollment := filepath.Join(dir, "client-profile.json")
	journal := filepath.Join(dir, "existing-workers.db")
	preferences := filepath.Join(dir, "navigation.json")
	projects := filepath.Join(dir, "projects")
	checkout := filepath.Join(projects, "demo")
	navigation, err := json.Marshal(map[string]any{"version": 1, "preferences": map[string]any{
		"projects_directory": projects,
		"project_checkouts": map[string]any{
			desktop.FavoriteKey("https://gateway.test", "demo"): map[string]string{"directory": checkout},
			desktop.FavoriteKey("https://other.test", "other"):  map[string]string{"directory": checkout},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preferences, navigation, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeClientConfig(context.Background(), settings, enrollment, "https://gateway.test", preferences, journal, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeClientConfig(context.Background(), settings, enrollment, "https://gateway.test", "", filepath.Join(dir, "different.db"), false, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.ReadClient(settings)
	if err != nil || loaded.WorkerJournal != journal || loaded.EnrolledProfile != enrollment ||
		loaded.ProjectsDirectory != projects || len(loaded.Projects) != 1 || loaded.Projects["demo"] != checkout {
		t.Fatalf("configuration migration returned %+v, %v", loaded, err)
	}
}

func TestChangingGatewayStopsPreviousClientBeforeReplacingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	current := config.Client{
		EnrolledProfile:  filepath.Join(dir, "old", "client-profile.json"),
		WorkerJournal:    filepath.Join(dir, "workers.db"),
		WorkPreferences:  filepath.Join(dir, "work-preferences.json"),
		TaskToolsCommand: filepath.Join(dir, "amux"),
		Projects:         map[string]string{"old-project": filepath.Join(dir, "old-checkout")},
		Providers:        []string{"codex"},
		ExcludedProjects: map[string]bool{"old-project": true},
	}
	if err := config.SaveClient(path, current); err != nil {
		t.Fatal(err)
	}
	lock, err := server.LockProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/shutdown" || r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_ = lock.Close()
		w.WriteHeader(http.StatusOK)
	})}
	go stop.Serve(listener)
	defer stop.Close()
	if err := os.WriteFile(path+".control", []byte(listener.Addr().String()+"\ntest-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "new", "client-profile.json")
	if err := writeClientConfig(context.Background(), path, replacement, "https://new.test", "", "", true, nil); err != nil {
		t.Fatal(err)
	}
	settings, err := config.ReadClient(path)
	if err != nil || settings.EnrolledProfile != replacement || settings.WorkerJournal != current.WorkerJournal ||
		settings.WorkPreferences != current.WorkPreferences || settings.TaskToolsCommand != current.TaskToolsCommand ||
		len(settings.Projects) != 0 || len(settings.Providers) != 1 || settings.Providers[0] != "codex" ||
		!settings.ExcludedProjects["old-project"] {
		t.Fatalf("unexpected replacement configuration: %+v, %v", settings, err)
	}
}
