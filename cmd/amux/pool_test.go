package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPoolHelpAndResumeFlags(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"worker", "pool", "--help"}, &output, &output, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "pool-id") || !strings.Contains(output.String(), "slots") || !strings.Contains(output.String(), "goal") {
		t.Fatalf("pool help: %s", output.String())
	}
	for _, flag := range []string{"--provider=codex", "--slots=2", "--directory=/tmp", "--project=demo", "--agent-command=[\"agent\"]", "--goal=release"} {
		err := runPool(context.Background(), []string{"--state=unused.db", "--pool-id=resume", flag}, &output, &output, func(string) string { return "test-key" })
		if err == nil || !strings.Contains(err.Error(), "cannot replace") {
			t.Fatalf("resume override %s: %v", flag, err)
		}
	}
}
