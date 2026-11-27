package desktopapp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestDesktopTaskToolsUsesExplicitExecutableWithoutLaunchingIt(t *testing.T) {
	name := "task tools"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("synthetic executable; never launched"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveTaskTools(path)
	if err != nil || got != path {
		t.Fatalf("resolve = %q, %v; want %q", got, err, path)
	}
}

func TestDesktopTaskToolsMissingExplicitCommandDoesNotFallback(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-amux")
	_, err := desktopWorkerOpen(missing, "http://127.0.0.1:1", "UNUSED_TEST_KEY")(context.Background(), storage.WorkerRun{}, nil)
	if err == nil || !strings.Contains(err.Error(), "locate amux task tools") {
		t.Fatalf("missing tools error = %v", err)
	}
}

func TestDesktopWorkerTaskToolsAcceptEnrolledProfileWithoutBearerEnvironment(t *testing.T) {
	name := "amux"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(executable, []byte("placeholder"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "local-client.json")
	open := desktopWorkerOpenWithEnrolledProfile(executable, "https://gateway.example", "CUSTOM_BEARER", "", manifest)
	_, err := open(context.Background(), storage.WorkerRun{
		Server: "https://gateway.example",
		Owner:  storage.AgentOrigin{Provider: "unsupported"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), `provider "unsupported" is not implemented`) {
		t.Fatalf("enrolled task tools did not proceed without bearer credentials: %v", err)
	}
}
