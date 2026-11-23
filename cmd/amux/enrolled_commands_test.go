package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestEnrolledProfileConsoleCommands(t *testing.T) {
	profile := t.TempDir()
	_, stop := startServerCommand(t, "--profile", profile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", uuid.NewString(), "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stop()
	manifest := filepath.Join(profile, "local-client.json")
	store, err := storage.Open(context.Background(), filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		if name == "GATEWAY_API_KEY" || name == "AMUX_GATEWAY_KEY" {
			return "bearer-must-not-be-used"
		}
		return ""
	}
	ctx := context.Background()

	var output, stderr bytes.Buffer
	if err := runTasks(ctx, []string{"--project", "demo", "--enrolled-profile", manifest, "list"}, &output, &stderr, getenv); err != nil {
		t.Fatalf("tasks list through enrolled profile: %v (%s)", err, stderr.String())
	}
	if !strings.Contains(output.String(), "[]") {
		t.Fatalf("unexpected task list: %s", output.String())
	}

	output.Reset()
	stderr.Reset()
	if err := runMessages(ctx, []string{"--project", "demo", "--enrolled-profile", manifest, "list"}, &output, &stderr, func(name string) string {
		if name == "AMUX_GATEWAY_URL" {
			return ""
		}
		return getenv(name)
	}); err != nil {
		t.Fatalf("messages list through enrolled profile: %v (%s)", err, stderr.String())
	}

	ghDir := t.TempDir()
	ghPath := filepath.Join(ghDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nif [ \"$1\" = repo ]; then\n  printf '%s\\n' '{\"nameWithOwner\":\"fixture/repo\"}'\nelse\n  printf '%s\\n' '{\"workflow_runs\":[]}'\nfi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", ghDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	output.Reset()
	stderr.Reset()
	if err := runTasks(ctx, []string{"--project", "demo", "--enrolled-profile", manifest, "report", "--format", "json", "--since", "2020-01-01", "--until", "2030-01-01"}, &output, &stderr, getenv); err != nil {
		t.Fatalf("task report through enrolled profile: %v (%s)", err, stderr.String())
	}

	repo := t.TempDir()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("init fixture checkout: %v: %s", err, out)
	}
	output.Reset()
	stderr.Reset()
	err = runSubmitWorkAlias(ctx, []string{
		"--enrolled-profile", manifest, "--project", "demo", "--directory", repo,
		"--session", uuid.NewString(), "--instance", uuid.NewString(), "--provider", "codex", "--os", "linux",
		uuid.NewString(), "fixture submission",
	}, &output, &stderr, getenv)
	if err == nil || !strings.Contains(err.Error(), "verify task ownership before reading paths") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("submit-work did not reach enrolled task ownership verification: %v", err)
	}
}
