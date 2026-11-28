package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestLegacyEntryPointsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"mcp"}, {"client", "run"}, {"server"}, {"server", "run"}, {"server", "--database", "amux.db"}, {"server", "--listen", "127.0.0.1:0"}, {"server", "peering-key"},
		{"worker", "run"}, {"messages"}, {"tasks"}, {"submit-work"}, {"work-submission"},
		{"--update"}, {"--finish-update"}, {"--reload-config"}, {"--generate-client-config"}, {"--generate-server-config"},
	} {
		cmd := newCommand()
		cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
		if err := cmd.Run(context.Background(), append([]string{"amux"}, args...)); err == nil {
			t.Fatalf("accepted removed CLI entry point %q", args)
		}
	}
}

func TestDesktopRejectsRemovedStartupFlags(t *testing.T) {
	for _, name := range []string{
		"project", "project-dir", "conversation", "preferences", "work-preferences",
		"gateway-preferences", "gateway-state-dir", "state", "run-id",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newCommand()
			cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
			err := cmd.Run(context.Background(), []string{"amux", "--" + name + "=example"})
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("expected the CLI to reject --%s, got %v", name, err)
			}
		})
	}
}
