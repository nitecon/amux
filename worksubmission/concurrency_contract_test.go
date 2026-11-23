package worksubmission_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/worksubmission"
)

type submitResult struct {
	out *worksubmission.Outcome
	err error
}

type postGitCommandBarrier struct {
	readyPath    string
	continuePath string
}

func installPostGitCommandBarrier(t *testing.T, realGit, commandName string) postGitCommandBarrier {
	t.Helper()
	basePath := os.Getenv("PATH")
	wrapperDir := t.TempDir()
	barrierDir := t.TempDir()
	barrier := postGitCommandBarrier{
		readyPath:    filepath.Join(barrierDir, "ready"),
		continuePath: filepath.Join(barrierDir, "continue"),
	}
	capturePath := filepath.Join(barrierDir, "captured-stdout")
	wrapper := filepath.Join(wrapperDir, "git")
	script := `saw_target=0
for arg in "$@"; do
	if [ "$arg" = "$AMUX_SUBMIT_BARRIER_COMMAND" ]; then saw_target=1; fi
done
if [ "$saw_target" -eq 1 ]; then
	"$AMUX_SUBMIT_REAL_GIT" "$@" > "$AMUX_SUBMIT_CAPTURE"
	status=$?
	if [ "$status" -eq 0 ]; then
		: > "$AMUX_SUBMIT_READY"
		while [ ! -e "$AMUX_SUBMIT_CONTINUE" ]; do /bin/sleep 0.01; done
	fi
	/bin/cat "$AMUX_SUBMIT_CAPTURE"
	exit "$status"
fi
exec "$AMUX_SUBMIT_REAL_GIT" "$@"
`
	script = "#!/bin/sh\n" + script
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+basePath)
	t.Setenv("AMUX_SUBMIT_BARRIER_COMMAND", commandName)
	t.Setenv("AMUX_SUBMIT_READY", barrier.readyPath)
	t.Setenv("AMUX_SUBMIT_CONTINUE", barrier.continuePath)
	t.Setenv("AMUX_SUBMIT_CAPTURE", capturePath)
	t.Setenv("AMUX_SUBMIT_REAL_GIT", realGit)
	t.Cleanup(func() { _ = os.WriteFile(barrier.continuePath, nil, 0600) })
	return barrier
}

func (b postGitCommandBarrier) wait(t *testing.T, done <-chan submitResult) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(b.readyPath); err == nil {
			return
		}
		select {
		case finished := <-done:
			t.Fatalf("submission finished before the post-command barrier: outcome=%+v err=%v", finished.out, finished.err)
		case <-time.After(10 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("submission did not reach the post-command barrier")
			}
		}
	}
}

func (b postGitCommandBarrier) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(b.continuePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSourceConcurrentCommitAndRebaseDoNotPublishForeignHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the deterministic PATH wrapper uses a POSIX shell")
	}

	// Resolve Git before either subtest installs its PATH wrapper.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err = filepath.Abs(realGit)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name           string
		barrierCommand string
		advanceRemote  bool
	}{
		{name: "after_task_commit", barrierCommand: "commit"},
		{name: "after_successful_rebase", barrierCommand: "rebase", advanceRemote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSourceFixture(t)
			if tc.advanceRemote {
				f.advance("c.txt", "upstream advanced before submission\n")
			}
			f.write("a.txt", "task-owned change\n")
			barrier := installPostGitCommandBarrier(t, realGit, tc.barrierCommand)

			done := make(chan submitResult, 1)
			go func() {
				out, err := worksubmission.Submit(context.Background(), worksubmission.Request{
					Root: f.root, TaskID: sourceTask, Message: "Task change", Paths: []string{"a.txt"},
				}, worksubmission.DefaultPreferences(), nil)
				done <- submitResult{out: out, err: err}
			}()
			barrier.wait(t, done)

			runRealGit := func(args ...string) string {
				t.Helper()
				command := exec.Command(realGit, append([]string{"-C", f.root}, args...)...)
				for _, variable := range os.Environ() {
					if !strings.HasPrefix(strings.ToUpper(variable), "GIT_") {
						command.Env = append(command.Env, variable)
					}
				}
				command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("real Git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}

			// The selected task commit (or rebased task commit) has succeeded,
			// but its Git command has not returned stdout to Submit yet.
			f.write("b.txt", "foreign concurrent change\n")
			runRealGit("add", "--", "b.txt")
			runRealGit("commit", "-m", "unrelated concurrent commit")
			foreignCommit := runRealGit("rev-parse", "HEAD")
			taskCommit := runRealGit("rev-parse", "HEAD^")
			barrier.release(t)

			var submitted submitResult
			select {
			case submitted = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("submission did not leave the post-command barrier")
			}
			if localHead := runRealGit("rev-parse", "HEAD"); localHead != foreignCommit {
				t.Fatalf("submission changed the competing local HEAD: got=%s want=%s", localHead, foreignCommit)
			}

			remoteCommand := exec.Command(realGit, "-C", f.remote, "rev-parse", "refs/heads/main")
			remoteBytes, err := remoteCommand.Output()
			if err != nil {
				t.Fatalf("read synthetic remote head: %v", err)
			}
			remoteHead := strings.TrimSpace(string(remoteBytes))
			if submitted.err != nil {
				if remoteHead == foreignCommit {
					t.Fatalf("safe error still published the competing commit: err=%v remote=%s", submitted.err, remoteHead)
				}
				t.Logf("submission safely refused publication after the concurrent commit: %v", submitted.err)
				return
			}
			if submitted.out == nil || submitted.out.Reference != taskCommit || remoteHead != taskCommit {
				reference := "<nil>"
				if submitted.out != nil {
					reference = submitted.out.Reference
				}
				t.Fatalf("submission did not publish exactly its own task commit: outcome=%s remote=%s want_task_commit=%s foreign_commit=%s", reference, remoteHead, taskCommit, foreignCommit)
			}
			t.Logf("submission published only its task commit %s; competing commit %s remains local", taskCommit, foreignCommit)
		})
	}
}
