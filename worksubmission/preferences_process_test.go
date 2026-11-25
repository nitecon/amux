package worksubmission

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

const processWriterEnv = "AMUX_TEST_PREFERENCES_WRITER"

// TestPreferencesProcessWriter is the child body for the cross-process test.
// It is a no-op unless launched by that test.
func TestPreferencesProcessWriter(t *testing.T) {
	role := os.Getenv(processWriterEnv)
	if role == "" {
		t.Skip("child process helper")
	}
	path := os.Getenv("AMUX_TEST_PREFERENCES_PATH")
	count, _ := strconv.Atoi(os.Getenv("AMUX_TEST_PREFERENCES_COUNT"))
	for i := range count {
		var err error
		if role == "save" {
			err = SavePreferences(path, Preferences{Mode: PullRequestMode, BranchPrefix: fmt.Sprintf("p%d/", i)})
		} else {
			err = AddToolApproval(path, fmt.Sprintf("%s-%d", role, i))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestSeparateProcessWritersKeepEveryUpdate models the CLI, desktop and inline
// approval writers sharing one preferences file from separate processes.
func TestSeparateProcessWritersKeepEveryUpdate(t *testing.T) {
	if os.Getenv(processWriterEnv) != "" {
		t.Skip("parent test")
	}
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	const count = 40
	roles := []string{"cli", "desktop", "save"}
	commands := make([]*exec.Cmd, 0, len(roles))
	for _, role := range roles {
		command := exec.Command(os.Args[0], "-test.run=^TestPreferencesProcessWriter$", "-test.count=1")
		command.Env = append(os.Environ(), processWriterEnv+"="+role,
			"AMUX_TEST_PREFERENCES_PATH="+path, "AMUX_TEST_PREFERENCES_COUNT="+strconv.Itoa(count))
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("writer process failed: %v", err)
		}
	}
	rules, err := LoadToolApprovals(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string]bool{}
	for _, rule := range rules {
		saved[rule] = true
	}
	for _, role := range roles[:2] {
		for i := range count {
			if rule := fmt.Sprintf("%s-%d", role, i); !saved[rule] {
				t.Errorf("acknowledged rule %q was lost", rule)
			}
		}
	}
	if preferences, err := LoadPreferences(path); err != nil || preferences.BranchPrefix != fmt.Sprintf("p%d/", count-1) {
		t.Fatalf("acknowledged preference save lost: %+v, %v", preferences, err)
	}
}
