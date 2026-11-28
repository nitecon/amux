package amuxruntime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/worksubmission"
)

func TestWorkSubmissionPreferencesCommandWritesOnlyWhenSetIsRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	if _, err := worksubmission.LoadPreferences(path); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := runWorkSubmissionSettings([]string{"--work-preferences", path, "set", "--mode", "pull_request", "--branch-prefix", "review/"}, &output, &output, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	got, err := worksubmission.LoadPreferences(path)
	if err != nil || got.Mode != worksubmission.PullRequestMode || got.BranchPrefix != "review/" {
		t.Fatalf("settings command wrote %+v, %v", got, err)
	}
}

func TestWorkSubmissionPreferencesConcurrentDisjointSetsKeepBothFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	const count = 30
	run := func(args ...string) error {
		var output strings.Builder
		return runWorkSubmissionSettings(append([]string{"--work-preferences", path, "set"}, args...), &output, &output, func(string) string { return "" })
	}
	errs := make(chan error, 2)
	go func() {
		for i := range count {
			mode := "commit_and_push"
			if i%2 == 1 {
				mode = "pull_request"
			}
			if err := run("--mode", mode); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	go func() {
		for i := range count {
			if err := run("--branch-prefix", fmt.Sprintf("p%d/", i)); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	want := worksubmission.Preferences{Mode: worksubmission.PullRequestMode, BranchPrefix: fmt.Sprintf("p%d/", count-1)}
	if got, err := worksubmission.LoadPreferences(path); err != nil || got != want {
		t.Fatalf("concurrent disjoint CLI sets lost an update: %+v, %v", got, err)
	}
}

func TestWorkSubmissionHelpIsAvailableWithoutServerOrPreferences(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{"alias", []string{"submit-work", "--help"}, []string{"usage: amux submit-work", "--server", "--directory", "paths-file"}},
		{"settings", []string{"work-submission", "--help"}, []string{"usage: amux work-submission", "--work-preferences", "--mode", "--branch-prefix"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if err := run(context.Background(), test.args, &stdout, &stderr, func(string) string { return "" }); err != nil {
				t.Fatalf("help returned an error: %v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("help polluted stdout: %q", stdout.String())
			}
			for _, want := range test.want {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("help output %q omitted %q", stderr.String(), want)
				}
			}
		})
	}
	var stdout, stderr strings.Builder
	if err := runTasks(context.Background(), []string{"submit-work", "--help"}, &stdout, &stderr, func(string) string { return "" }); err != nil {
		t.Fatalf("task submit-work help returned an error: %v", err)
	}
	if !strings.Contains(stderr.String(), "--directory CHECKOUT") || !strings.Contains(stderr.String(), "--resume-commit SHA") || !strings.Contains(stderr.String(), "--paths-file JSON") {
		t.Fatalf("task submit-work help is incomplete: %q", stderr.String())
	}
	if hasTaskHelp([]string{"--", "--help"}) {
		t.Fatal("literal --help after the end-of-options marker was treated as a help request")
	}
}
