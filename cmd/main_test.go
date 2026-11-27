package main

import (
	"context"
	"io"
	"testing"
)

func TestLegacyEntryPointsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"mcp"}, {"client", "run"}, {"server", "run"}, {"server", "peering-key"},
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
