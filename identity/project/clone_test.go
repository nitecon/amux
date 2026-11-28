package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func cloneGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestCloneRepositoryReusesCanonicalRemoteAcrossTransports(t *testing.T) {
	parent := t.TempDir()
	checkout := filepath.Join(parent, "project")
	cloneGit(t, parent, "init", checkout)
	cloneGit(t, checkout, "remote", "add", "origin", "git@example.org:team/project.git")
	cloneGit(t, checkout, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	keep := filepath.Join(checkout, "keep")
	if err := os.WriteFile(keep, []byte("local changes"), 0600); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	remote := "https://example.org/team/project.git"
	if got, err := CloneRepository(ctx, parent, remote); err != nil || got != want {
		t.Fatalf("matching remote reuse = %q, %v; want %q", got, err, want)
	}
	if _, err := CloneRepository(ctx, parent, "https://example.org/other/project.git"); err == nil {
		t.Fatal("reused checkout for a different canonical remote")
	}
	cloneGit(t, checkout, "remote", "remove", "origin")
	if _, err := CloneRepository(ctx, parent, remote); err == nil {
		t.Fatal("reused checkout with missing origin for a remote request")
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "local changes" {
		t.Fatalf("local changes = %q, %v", data, err)
	}
}

func TestCloneOriginReadsConfiguredURLWithoutCanonicalizingOrLeakingHTTPCredentials(t *testing.T) {
	directory := t.TempDir()
	cloneGit(t, directory, "init", "--quiet")
	raw := "https://user:secret@example.org/Team/Project.git?token=fixture"
	cloneGit(t, directory, "remote", "add", "origin", raw)
	origin, err := CloneOrigin(context.Background(), directory)
	if err != nil || origin == nil || *origin != "https://example.org/Team/Project.git" {
		t.Fatalf("clone origin=%v, err=%v", origin, err)
	}
	cloneGit(t, directory, "remote", "remove", "origin")
	origin, err = CloneOrigin(context.Background(), directory)
	if err != nil || origin != nil {
		t.Fatalf("missing origin=%v, err=%v; want nil", origin, err)
	}
	cloneGit(t, directory, "remote", "add", "origin", "ssh://git@example.org/Team/Project.git")
	origin, err = CloneOrigin(context.Background(), directory)
	if err != nil || origin == nil || *origin != "ssh://git@example.org/Team/Project.git" {
		t.Fatalf("SSH clone origin=%v, err=%v", origin, err)
	}
	cloneGit(t, directory, "remote", "set-url", "origin", "ssh://deploy@example.org/Team/Project.git")
	origin, err = CloneOrigin(context.Background(), directory)
	if err != nil || origin != nil {
		t.Fatalf("nonstandard SSH user origin=%v, err=%v; want nil", origin, err)
	}
}

func TestCloneRepositoryRejectsNonCheckoutWithoutModifyingIt(t *testing.T) {
	parent := t.TempDir()
	remote := "https://example.org/team/project.git"
	cloneGit(t, parent, "init")
	cloneGit(t, parent, "remote", "add", "origin", remote)
	destination := filepath.Join(parent, "project")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(destination, "keep")
	if err := os.WriteFile(keep, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	assertRejected := func(path string, want string) {
		t.Helper()
		if _, err := CloneRepository(context.Background(), parent, remote); err == nil {
			t.Fatal("clone accepted a destination that is not a checkout root")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("existing data changed: %q, %v", got, err)
		}
	}
	assertRejected(keep, "untouched")
	if err := os.Remove(keep); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("existing file"), 0600); err != nil {
		t.Fatal(err)
	}
	assertRejected(destination, "existing file")
}

func TestCloneRepositoryFailurePreservesFolderAndExplicitRemovalAllowsRetry(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "clones")
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "later.git")
	if _, err := CloneRepository(context.Background(), parent, source); err == nil {
		t.Fatal("clone from missing local source unexpectedly succeeded")
	}
	destination := filepath.Join(parent, "later")
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("failed clone folder was not preserved: %v", err)
	}
	keep := filepath.Join(destination, "keep")
	if err := os.WriteFile(keep, []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CloneRepository(context.Background(), parent, source); err == nil {
		t.Fatal("retry reused incomplete folder")
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "user data" {
		t.Fatalf("user data changed: %q, %v", got, err)
	}
	if err := os.RemoveAll(destination); err != nil {
		t.Fatal(err)
	}
	cloneGit(t, root, "init", "--bare", source)
	got, err := CloneRepository(context.Background(), parent, source)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(destination)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("destination = %q, want %q", got, want)
	}
}

func TestCloneRepositoryReusesVerifiedCheckoutAndPreservesLocalFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.git")
	cloneGit(t, root, "init", "--bare", source)
	parent := filepath.Join(root, "clone parent")
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	checkout, err := CloneRepository(context.Background(), parent, source)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(checkout, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("clone did not create Git metadata: %v", err)
	}
	keep := filepath.Join(checkout, "keep")
	if err := os.WriteFile(keep, []byte("local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	reused, err := CloneRepository(context.Background(), parent, source)
	if err != nil || reused != checkout {
		t.Fatalf("reuse = %q, %v; want %q", reused, err, checkout)
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "local edit" {
		t.Fatalf("local edit changed: %q, %v", got, err)
	}
}

func TestCloneRepositoryReusesExactLocalCheckoutWithoutOrigin(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "projects")
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(parent, "project")
	if err := os.Mkdir(checkout, 0755); err != nil {
		t.Fatal(err)
	}
	cloneGit(t, checkout, "init")
	got, err := CloneRepository(context.Background(), parent, checkout)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("reused path = %q, want %q", got, want)
	}
	otherParent := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(otherParent, 0755); err != nil {
		t.Fatal(err)
	}
	cloned, err := CloneRepository(context.Background(), otherParent, checkout)
	if err != nil {
		t.Fatal(err)
	}
	if cloned == want {
		t.Fatalf("separate clone reused source path %q", cloned)
	}
	if info, err := os.Stat(filepath.Join(cloned, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("unborn local project was not cloned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cloned, cloneMarker)); err != nil {
		t.Fatalf("empty clone lacks completion marker: %v", err)
	}
}

func TestCloneRepositoryRejectsIncompleteLegacyAndReservationSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "clones")
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source.git")
	destination := filepath.Join(parent, "source")
	cloneGit(t, root, "init", "--bare", source)
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	cloneGit(t, destination, "init")
	cloneGit(t, destination, "remote", "add", "origin", source)
	keep := filepath.Join(destination, "keep")
	if err := os.WriteFile(keep, []byte("partial data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CloneRepository(context.Background(), parent, source); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("legacy checkout without HEAD should fail as incomplete, got %v", err)
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "partial data" {
		t.Fatalf("legacy checkout file changed: %q, %v", got, err)
	}
	reservation := filepath.Join(parent, ".symlink.amux-clone-incomplete")
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := CloneRepository(context.Background(), parent, filepath.Join(root, "symlink.git")); err == nil {
		t.Fatal("symlink reservation was accepted")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "untouched" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
}

func TestCloneRepositoryPopulatesEmptyDestinationAndHonorsReservation(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "clones")
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source.git")
	cloneGit(t, root, "init", "--bare", source)
	destination := filepath.Join(parent, "source")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	reservation := filepath.Join(parent, ".source.amux-clone-incomplete")
	if err := os.WriteFile(reservation, []byte("another clone"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CloneRepository(context.Background(), parent, source); err == nil {
		t.Fatal("empty destination with an existing reservation was accepted")
	}
	if entries, err := os.ReadDir(destination); err != nil || len(entries) != 0 {
		t.Fatalf("existing empty destination changed: entries=%v err=%v", entries, err)
	}
	if content, err := os.ReadFile(reservation); err != nil || string(content) != "another clone" {
		t.Fatalf("existing sidecar changed: %q, %v", content, err)
	}

	file, err := os.OpenFile(reservation, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := tryCloneLock(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if _, err := CloneRepository(context.Background(), parent, source); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("concurrent reservation was not rejected: %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination was unexpectedly recreated while locked: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(reservation); err != nil || string(content) != "another clone" {
		t.Fatalf("locked sidecar changed: %q, %v", content, err)
	}
	if err := os.Remove(reservation); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	checkout, err := CloneRepository(context.Background(), parent, source)
	if err != nil {
		t.Fatalf("clone failed after explicit reservation cleanup: %v", err)
	}
	wantDestination, err := filepath.EvalSymlinks(destination)
	if err != nil {
		t.Fatal(err)
	}
	if checkout != wantDestination {
		t.Fatalf("clone checkout = %q, want pre-existing empty destination %q", checkout, wantDestination)
	}
	if content, err := os.ReadFile(reservation); err != nil || len(content) != 0 {
		t.Fatalf("persistent lock sidecar after success = %q, %v; want empty file", content, err)
	}
}

func TestCloneRepositoryConcurrentAndInterruptedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the process-gating shim uses a local POSIX shell")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	cloneGit(t, source, "init")
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("committed payload\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cloneGit(t, source, "add", "payload")
	cloneGit(t, source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(root, "bin")
	if err := os.Mkdir(shimDir, 0755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shimDir, "git")
	script := "#!/bin/sh\nif [ \"$1\" = clone ]; then\n  exec \"$AMUX_CLONE_TEST_BINARY\" -test.run='^TestCloneProcessGateHelper$' -- \"$@\"\nfi\nexec \"$AMUX_CLONE_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AMUX_CLONE_TEST_BINARY", os.Args[0])
	t.Setenv("AMUX_CLONE_REAL_GIT", gitPath)
	for _, completes := range []bool{true, false} {
		caseName := "interrupted"
		if completes {
			caseName = "completed"
		}
		parent := filepath.Join(root, "clones-"+caseName)
		if err := os.Mkdir(parent, 0755); err != nil {
			t.Fatal(err)
		}
		ready := filepath.Join(root, caseName+"-ready")
		release := filepath.Join(root, caseName+"-release")
		t.Setenv("AMUX_CLONE_READY", ready)
		t.Setenv("AMUX_CLONE_RELEASE", release)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		done := make(chan struct{})
		go func() {
			_, cloneErr := CloneRepository(ctx, parent, source)
			result <- cloneErr
			close(done)
		}()
		func() {
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("clone subprocess did not stop during cleanup")
				}
			}()

			deadline := time.Now().Add(15 * time.Second)
			for {
				if _, statErr := os.Stat(ready); statErr == nil {
					break
				}
				select {
				case cloneErr := <-result:
					t.Fatalf("clone exited before reaching gate: %v", cloneErr)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("clone process did not reach deterministic gate")
				}
				time.Sleep(10 * time.Millisecond)
			}

			destination := filepath.Join(parent, "source")
			if _, statErr := os.Stat(filepath.Join(destination, ".git")); statErr != nil {
				t.Fatalf("checkout did not reach Git metadata before gate: %v", statErr)
			}
			if _, cloneErr := CloneRepository(context.Background(), parent, source); cloneErr == nil || !strings.Contains(cloneErr.Error(), "already running") {
				t.Fatalf("concurrent clone was not rejected while process held lock: %v", cloneErr)
			}
			verify := exec.Command(gitPath, "-C", destination, "rev-parse", "--verify", "HEAD^{commit}")
			if output, verifyErr := verify.CombinedOutput(); verifyErr != nil {
				t.Fatalf("gated clone has no HEAD: %v\n%s", verifyErr, output)
			}

			if completes {
				if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
					t.Fatal(err)
				}
				if cloneErr := <-result; cloneErr != nil {
					t.Fatalf("released clone failed: %v", cloneErr)
				}
				payload, readErr := os.ReadFile(filepath.Join(destination, "payload"))
				if readErr != nil || string(payload) != "committed payload\n" {
					t.Fatalf("completed checkout payload = %q, %v", payload, readErr)
				}
				if err := os.WriteFile(filepath.Join(destination, "local-edit"), []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, cloneErr := CloneRepository(context.Background(), parent, source); cloneErr != nil {
					t.Fatalf("completed checkout was not reusable: %v", cloneErr)
				}
				if contents, readErr := os.ReadFile(filepath.Join(destination, "local-edit")); readErr != nil || string(contents) != "preserved" {
					t.Fatalf("local edit was not preserved: %q, %v", contents, readErr)
				}
			} else {
				// The underlying clone has written HEAD and checkout files; kill only
				// the fixture-owned gate process before the API records completion.
				cancel()
				if cloneErr := <-result; cloneErr == nil {
					t.Fatal("canceled clone unexpectedly succeeded")
				}
				if _, statErr := os.Stat(filepath.Join(destination, ".git", "amux-clone-complete")); !os.IsNotExist(statErr) {
					t.Fatalf("interrupted clone has a completion marker: %v", statErr)
				}
				if _, cloneErr := CloneRepository(context.Background(), parent, source); cloneErr == nil || !strings.Contains(cloneErr.Error(), "incomplete") {
					t.Fatalf("interrupted checkout was not refused on retry: %v", cloneErr)
				}
			}
		}()
	}
}

// TestCloneProcessGateHelper is invoked as a subprocess by the gated clone
// fixture. The underlying local Git clone finishes before the helper signals
// readiness; the helper then holds the parent command open until released or
// killed by the test.
func TestCloneProcessGateHelper(t *testing.T) {
	if os.Getenv("AMUX_CLONE_TEST_BINARY") == "" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatal("clone helper did not receive Git arguments")
	}
	command := exec.Command(os.Getenv("AMUX_CLONE_REAL_GIT"), os.Args[separator+1:]...)
	command.Env = cloneEnvironment()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture local git clone failed: %v\n%s", err, output)
	}
	if err := os.WriteFile(os.Getenv("AMUX_CLONE_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv("AMUX_CLONE_RELEASE")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("clone gate timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestValidateCloneSourceRejectsCommandTransport(t *testing.T) {
	for _, source := range []string{"ext::sh -c evil", "git+https://example.org/team/repo.git", "-upload-pack=evil"} {
		if err := validateCloneSource(source); err == nil {
			t.Errorf("accepted unsafe source %q", source)
		}
	}
	for _, source := range []string{"git@example.org:team/repo.git", "https://example.org/team/repo.git", filepath.Join(t.TempDir(), "repo.git")} {
		if err := validateCloneSource(source); err != nil {
			t.Errorf("rejected supported source %q: %v", source, err)
		}
	}
}

func TestObserveTimeoutReportedDistinctlyFromNonCheckout(t *testing.T) {
	if observeTimeout < 10*time.Second {
		t.Fatalf("observeTimeout = %v, want at least 10s for loaded hosts", observeTimeout)
	}
	parent := t.TempDir()
	checkout := filepath.Join(parent, "project")
	cloneGit(t, parent, "init", checkout)
	cloneGit(t, checkout, "remote", "add", "origin", "git@example.org:team/project.git")

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := CloneRepository(expired, parent, "https://example.org/team/project.git")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloneRepository deadline error = %v, want context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "not a checkout of the requested repository") {
		t.Fatalf("CloneRepository misreported deadline as non-checkout: %v", err)
	}
	_, err = ResolveCheckout(expired, checkout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ResolveCheckout deadline error = %v, want context.DeadlineExceeded", err)
	}
}
