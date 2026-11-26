// Package worksubmission commits and publishes only the paths owned by a task.
package worksubmission

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nitecon/amux/internal/hidewindow"
)

type Mode string

const (
	CommitAndPush   Mode = "commit_and_push"
	PullRequestMode Mode = "pull_request"
)

type Preferences struct {
	Mode         Mode   `json:"mode"`
	BranchPrefix string `json:"branch_prefix"`
}

func DefaultPreferences() Preferences { return Preferences{Mode: CommitAndPush, BranchPrefix: "amux/"} }

type Request struct {
	Root, TaskID, Message string
	Paths                 []string
}
type Outcome struct {
	Reference string
	Files     []string
}

func (o Outcome) Comment() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Work submitted: %s\n\nFiles:", o.Reference)
	for _, f := range o.Files {
		b.WriteString("\n- ")
		b.WriteString(f)
	}
	return b.String()
}

type PullRequest struct{ Root, Head, Base, Title, Body string }
type OpenPullRequest func(context.Context, PullRequest) (string, error)

var transportEnv = map[string]bool{"GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_SSH_VARIANT": true, "GIT_ASKPASS": true}
var strippedTrailers = map[string]bool{"co-authored-by": true, "co-developed-by": true, "assisted-by": true, "generated-by": true, "amuxid": true}
var commitSummaryID = regexp.MustCompile(`(?m)^\[[^\r\n]* ([0-9a-f]{40}|[0-9a-f]{64})\].*$`)

var (
	errTaskTrailerMismatch = errors.New("AmuxID trailer does not match the submitted task")
	errTaskNotSingleParent = errors.New("commit is not a single-parent task commit")
)

func Submit(ctx context.Context, req Request, prefs Preferences, open OpenPullRequest) (*Outcome, error) {
	if err := validateMode(prefs.Mode); err != nil {
		return nil, err
	}
	if prefs.Mode == PullRequestMode && open == nil {
		return nil, errors.New("pull request opener is required")
	}
	repo, taskID, err := prepare(req)
	if err != nil {
		return nil, err
	}
	message := cleanMessage(req.Message)
	if !utf8.ValidString(message) || strings.IndexByte(message, 0) >= 0 {
		return nil, errors.New("commit message must be valid UTF-8 without NUL characters")
	}
	if message == "" {
		return nil, errors.New("the commit message is empty once attribution lines are removed")
	}
	message += "\n\nAmuxID: " + taskID
	files, err := repo.changedPaths(ctx, req.Paths)
	if err != nil {
		return nil, err
	}
	up, err := repo.upstream(ctx)
	if err != nil {
		return nil, err
	}
	if prefs.Mode == CommitAndPush {
		if _, err = repo.gitPaths(ctx, []string{"add", "--all"}, files); err != nil {
			return nil, err
		}
		commitOutput, commitErr := repo.gitPathsRaw(ctx, []string{"-c", "core.abbrev=64", "-c", "color.ui=false", "commit", "--only", "--no-quiet", "--no-status", "-m", message}, files)
		if commitErr != nil {
			return nil, commitErr
		}
		commit := committedObjectID(commitOutput)
		if commit == "" {
			return nil, errors.New("git commit succeeded but its full commit ID was not reported; the commit was left local and was not published")
		}
		ref, e := repo.publish(ctx, up, commit, taskID, files)
		if e != nil {
			return nil, e
		}
		return &Outcome{Reference: ref, Files: files}, nil
	}
	branch := branchName(prefs.BranchPrefix, taskID)
	if _, err = repo.git(ctx, "check-ref-format", "refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("%q is not a valid branch name; change the pull request branch prefix in Settings: %w", branch, err)
	}
	pull, err := repo.buildAndPushPR(ctx, up, files, message, branch)
	if err != nil {
		return nil, err
	}
	ref, err := open(ctx, pull)
	if err != nil {
		return nil, fmt.Errorf("branch %s was pushed, but its pull request could not be opened: %w", branch, err)
	}
	return &Outcome{Reference: ref, Files: files}, nil
}

func Resume(ctx context.Context, req Request, prefs Preferences, commit string, open OpenPullRequest) (*Outcome, error) {
	if err := validateMode(prefs.Mode); err != nil {
		return nil, err
	}
	if prefs.Mode == PullRequestMode && open == nil {
		return nil, errors.New("pull request opener is required")
	}
	repo, taskID, err := prepare(req)
	if err != nil {
		return nil, err
	}
	commit, err = repo.git(ctx, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}")
	if err != nil {
		return nil, err
	}
	head, err := repo.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if head != commit {
		return nil, errors.New("resume commit must be the current HEAD; no work was published")
	}
	parent, err := repo.taskCommitParent(ctx, commit, taskID)
	if errors.Is(err, errTaskTrailerMismatch) {
		return nil, errors.New("resume commit must have exactly one matching AmuxID trailer")
	}
	if errors.Is(err, errTaskNotSingleParent) {
		return nil, errors.New("resume requires a single-parent task commit")
	}
	if err != nil {
		return nil, err
	}
	files, err := repo.pathspecs(req.Paths)
	if err != nil {
		return nil, err
	}
	selected, err := repo.selectedCommitPaths(ctx, files)
	if err != nil {
		return nil, err
	}
	filesMatch, err := repo.commitFilesMatch(ctx, commit, selected)
	if err != nil {
		return nil, err
	}
	if !filesMatch {
		return nil, errors.New("resume commit must change exactly the listed files")
	}
	unmerged, err := repo.gitRaw(ctx, "ls-files", "--unmerged", "-z")
	if err != nil {
		return nil, err
	}
	if len(unmerged) > 0 {
		return nil, errors.New("resolve all index conflicts before resuming publication")
	}
	status, err := repo.gitRaw(ctx, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	for _, path := range statusPaths(status) {
		if selected[path] {
			return nil, errors.New("listed files have uncommitted changes; resolve them before resuming publication")
		}
	}
	up, err := repo.upstream(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = repo.git(ctx, "fetch", up.remote); err != nil {
		return nil, err
	}
	base, err := repo.git(ctx, "rev-parse", "--verify", "@{upstream}^{commit}")
	if err != nil {
		return nil, err
	}
	if repo.isAncestor(ctx, commit, base) {
		return &Outcome{Reference: commit, Files: files}, nil
	}
	if !repo.isAncestor(ctx, parent, base) {
		return nil, errors.New("resume would publish other local history; reconcile the commit parent with upstream first")
	}
	if prefs.Mode == CommitAndPush {
		ref, e := repo.publish(ctx, up, commit, taskID, files)
		if e != nil {
			return nil, e
		}
		return &Outcome{Reference: ref, Files: files}, nil
	}
	branch := branchName(prefs.BranchPrefix, taskID)
	if _, err = repo.git(ctx, "check-ref-format", "refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("%q is not a valid branch name; change the pull request branch prefix in Settings: %w", branch, err)
	}
	message, err := repo.git(ctx, "log", "-1", "--format=%B", commit)
	if err != nil {
		return nil, err
	}
	pull, err := repo.pushPR(ctx, up, commit, message, branch)
	if err != nil {
		return nil, err
	}
	ref, err := open(ctx, pull)
	if err != nil {
		return nil, fmt.Errorf("branch %s was pushed, but its pull request could not be opened: %w", branch, err)
	}
	return &Outcome{Reference: ref, Files: files}, nil
}

func OpenGitHubPullRequest(ctx context.Context, p PullRequest) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "create", "--head="+p.Head, "--base="+p.Base, "--title="+p.Title, "--body="+p.Body)
	hidewindow.Apply(cmd)
	cmd.Dir = p.Root
	cmd.Env = cleanEnv()
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1")
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh pr create failed (exit status %d)", exit.ExitCode())
		}
		return "", errors.New("cannot run gh; install the GitHub CLI and sign in with gh auth login")
	}
	lines := strings.Split(out.String(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s, nil
		}
	}
	return "", errors.New("gh pr create did not print the pull request URL")
}

func validateMode(m Mode) error {
	if m != CommitAndPush && m != PullRequestMode {
		return fmt.Errorf("unknown work submission mode %q", m)
	}
	return nil
}

func committedObjectID(output []byte) string {
	matches := commitSummaryID.FindAllSubmatch(output, -1)
	if len(matches) == 0 {
		return ""
	}
	return string(matches[len(matches)-1][1])
}
func branchName(prefix string, id string) string { return strings.TrimSpace(prefix) + id }

func cleanMessage(message string) string {
	lines := strings.Split(message, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		lower := strings.ToLower(strings.TrimSpace(line))
		remove := false
		if k, _, ok := strings.Cut(lower, ":"); ok && strippedTrailers[strings.TrimSpace(k)] {
			remove = true
		}
		trim := strings.TrimLeftFunc(lower, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if strings.HasPrefix(trim, "generated with") {
			for _, a := range []string{"claude", "codex", "copilot", "gemini"} {
				if strings.Contains(trim, a) {
					remove = true
					break
				}
			}
		}
		if remove {
			continue
		}
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if line == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
func nulSet(b []byte) map[string]bool {
	m := map[string]bool{}
	for _, x := range bytes.Split(b, []byte{0}) {
		if len(x) > 0 {
			m[string(x)] = true
		}
	}
	return m
}

func nulList(b []byte) []string {
	var paths []string
	for _, value := range bytes.Split(b, []byte{0}) {
		if len(value) > 0 {
			paths = append(paths, string(value))
		}
	}
	return paths
}
func statusPaths(b []byte) []string {
	var paths []string
	for _, r := range bytes.Split(b, []byte{0}) {
		if len(r) >= 4 {
			paths = append(paths, string(r[3:]))
		}
	}
	return paths
}

func prepare(req Request) (*repo, string, error) {
	if req.Root == "" {
		return nil, "", errors.New("project root must not be empty")
	}
	id, err := uuid.Parse(req.TaskID)
	if err != nil {
		return nil, "", errors.New("task ID must be a UUID")
	}
	if !utf8.ValidString(req.Root) || strings.IndexByte(req.Root, 0) >= 0 {
		return nil, "", errors.New("project root is not valid UTF-8")
	}
	root, err := filepath.EvalSymlinks(req.Root)
	if err != nil {
		return nil, "", fmt.Errorf("cannot open project root %s: %w", req.Root, err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	return &repo{root: root}, id.String(), nil
}

type repo struct {
	root  string
	index string
}

func (r *repo) command(ctx context.Context, args ...string) *exec.Cmd {
	all := append([]string{"-C", r.root}, args...)
	c := exec.CommandContext(ctx, "git", all...)
	hidewindow.Apply(c)
	c.Env = cleanEnv()
	c.Env = append(c.Env, "GIT_LITERAL_PATHSPECS=1")
	if r.index != "" {
		c.Env = append(c.Env, "GIT_INDEX_FILE="+r.index)
	}
	return c
}
func cleanEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+2)
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(k, "GIT_") && !transportEnv[k] {
			continue
		}
		out = append(out, v)
	}
	out = append(out, "GIT_TERMINAL_PROMPT=0")
	return out
}
func (r *repo) run(ctx context.Context, args ...string) ([]byte, error) {
	c := r.command(ctx, args...)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		name := "git " + gitCommandName(args)
		if exit, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s failed (exit status %d)", name, exit.ExitCode())
		}
		return nil, fmt.Errorf("cannot run %s", name)
	}
	return out.Bytes(), nil
}

func gitCommandName(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return "command"
}

func (r *repo) git(ctx context.Context, args ...string) (string, error) {
	b, e := r.run(ctx, args...)
	return strings.TrimSpace(string(b)), e
}
func (r *repo) gitRaw(ctx context.Context, args ...string) ([]byte, error) {
	return r.run(ctx, args...)
}
func (r *repo) gitPaths(ctx context.Context, args []string, files []string) (string, error) {
	out, err := r.gitPathsRaw(ctx, args, files)
	return strings.TrimSpace(string(out)), err
}

func (r *repo) gitPathsRaw(ctx context.Context, args []string, files []string) ([]byte, error) {
	f, e := os.CreateTemp("", "amux-pathspec-*")
	if e != nil {
		return nil, e
	}
	defer os.Remove(f.Name())
	for _, p := range files {
		if _, e = f.Write(append([]byte(p), 0)); e != nil {
			f.Close()
			return nil, e
		}
	}
	if e = f.Close(); e != nil {
		return nil, e
	}
	a := append([]string{}, args...)
	a = append(a, "--pathspec-from-file="+f.Name(), "--pathspec-file-nul")
	return r.gitRaw(ctx, a...)
}

func (r *repo) pathspecs(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("list at least one changed path to submit")
	}
	out := []string{}
	seen := map[string]bool{}
	for _, p := range paths {
		f, e := r.pathspec(p)
		if e != nil {
			return nil, e
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}
func (r *repo) pathspec(path string) (string, error) {
	shown := path
	if !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%s is not valid UTF-8", shown)
	}
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.root, p)
	}
	p = filepath.Clean(p)
	base := filepath.Base(p)
	if base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("%s is not a file path", shown)
	}
	parent := filepath.Dir(p)
	missing := []string{}
	var resolved string
	for {
		v, e := filepath.EvalSymlinks(parent)
		if e == nil {
			resolved = v
			break
		}
		if _, se := os.Stat(parent); se == nil {
			return "", fmt.Errorf("%s parent cannot be resolved: %w", shown, e)
		}
		n := filepath.Base(parent)
		up := filepath.Dir(parent)
		if n == parent {
			return "", errors.New("path has no existing folder")
		}
		missing = append(missing, n)
		parent = up
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	resolved = filepath.Join(resolved, base)
	rel, e := filepath.Rel(r.root, resolved)
	if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is not inside the project at %s", shown, r.root)
	}
	if !utf8.ValidString(rel) {
		return "", fmt.Errorf("%s is not valid UTF-8", shown)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || strings.EqualFold(parts[0], ".git") {
		return "", fmt.Errorf("%s is inside the Git directory", shown)
	}
	return strings.Join(parts, "/"), nil
}
func (r *repo) changedPaths(ctx context.Context, paths []string) ([]string, error) {
	files, e := r.pathspecs(paths)
	if e != nil {
		return nil, e
	}
	b, e := r.gitRaw(ctx, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all")
	if e != nil {
		return nil, e
	}
	pre, e := r.gitRaw(ctx, "rev-parse", "--show-prefix")
	if e != nil {
		return nil, e
	}
	prefix := string(bytes.TrimRight(pre, "\r\n"))
	changed := map[string]bool{}
	for _, p := range statusPaths(b) {
		if strings.HasPrefix(p, prefix) {
			changed[strings.TrimPrefix(p, prefix)] = true
		}
	}
	for _, f := range files {
		if !changed[f] {
			return nil, fmt.Errorf("%s has no changes to submit", f)
		}
	}
	return files, nil
}

type upstream struct{ remote, merge string }

func (u upstream) branch() string { return strings.TrimPrefix(u.merge, "refs/heads/") }
func (r *repo) upstream(ctx context.Context) (upstream, error) {
	branch, e := r.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if e != nil {
		return upstream{}, errors.New("HEAD is detached; check out a branch with an upstream before submitting work")
	}
	remote, e1 := r.git(ctx, "config", "--get", "branch."+branch+".remote")
	merge, e2 := r.git(ctx, "config", "--get", "branch."+branch+".merge")
	if e1 != nil || e2 != nil || remote == "" || !strings.HasPrefix(merge, "refs/heads/") {
		return upstream{}, fmt.Errorf("branch %s has no upstream; set one with git push -u before submitting work", branch)
	}
	return upstream{remote, merge}, nil
}

func (r *repo) isAncestor(ctx context.Context, a, b string) bool {
	_, e := r.git(ctx, "merge-base", "--is-ancestor", a, b)
	return e == nil
}

func (r *repo) publish(ctx context.Context, u upstream, commit, taskID string, files []string) (string, error) {
	push := func(id string) error { _, e := r.git(ctx, "push", u.remote, id+":"+u.merge); return e }
	if e := push(commit); e == nil {
		return commit, nil
	} else {
		rejected := e
		if _, f := r.git(ctx, "fetch", u.remote); f != nil {
			return "", fmt.Errorf("commit %s is kept locally and was not pushed: %w", commit, f)
		}
		base, f := r.git(ctx, "rev-parse", "--verify", "@{upstream}^{commit}")
		if f != nil {
			return "", f
		}
		if r.isAncestor(ctx, commit, base) {
			return commit, nil
		}
		if r.isAncestor(ctx, base, commit) {
			return "", fmt.Errorf("%w; commit %s is kept locally and was not pushed", rejected, commit)
		}
		head, f := r.git(ctx, "rev-parse", "HEAD")
		if f != nil {
			return "", f
		}
		if head != commit {
			return "", fmt.Errorf("commit %s is kept locally and was not pushed; HEAD changed before rebase", commit)
		}
		if f = r.rebasePreservingIndex(ctx, u.branch()); f != nil {
			kept, note := commit, ""
			if current, headErr := r.git(ctx, "rev-parse", "HEAD"); headErr == nil {
				if current != commit {
					if validateErr := r.validateTaskCommit(ctx, current, taskID, files); validateErr == nil {
						kept = current
					} else {
						note = fmt.Sprintf("; HEAD changed to %s and could not be verified as this task commit", current)
					}
				}
			} else {
				note = "; current HEAD could not be verified"
			}
			return "", fmt.Errorf("commit %s is kept locally and was not pushed%s: %w", kept, note, f)
		}
		newHead, f := r.git(ctx, "rev-parse", "HEAD")
		if f != nil {
			return "", f
		}
		if f = r.validateTaskCommit(ctx, newHead, taskID, files); f != nil {
			return "", fmt.Errorf("rebased commit %s is not the expected task commit; it was left local and not pushed: %w", newHead, f)
		}
		if f = push(newHead); f != nil {
			return "", fmt.Errorf("commit %s is kept locally and was not pushed: %w", newHead, f)
		}
		return newHead, nil
	}
}

func (r *repo) validateTaskCommit(ctx context.Context, commit, taskID string, files []string) error {
	if _, err := r.taskCommitParent(ctx, commit, taskID); err != nil {
		return err
	}
	selected, err := r.selectedCommitPaths(ctx, files)
	if err != nil {
		return err
	}
	matches, err := r.commitFilesMatch(ctx, commit, selected)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("commit does not change exactly the submitted paths")
	}
	return nil
}

func (r *repo) taskCommitParent(ctx context.Context, commit, taskID string) (string, error) {
	trailers, err := r.git(ctx, "log", "-1", "--format=%(trailers:key=AmuxID,valueonly)", commit)
	if err != nil {
		return "", err
	}
	if trailers != taskID {
		return "", errTaskTrailerMismatch
	}
	parents, err := r.git(ctx, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(parents)
	if len(fields) != 2 {
		return "", errTaskNotSingleParent
	}
	return fields[1], nil
}

func (r *repo) selectedCommitPaths(ctx context.Context, files []string) (map[string]bool, error) {
	prefixBytes, err := r.gitRaw(ctx, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, err
	}
	prefix := string(bytes.TrimRight(prefixBytes, "\r\n"))
	selected := make(map[string]bool, len(files))
	for _, file := range files {
		selected[prefix+file] = true
	}
	return selected, nil
}

func (r *repo) commitFilesMatch(ctx context.Context, commit string, selected map[string]bool) (bool, error) {
	changedBytes, err := r.gitRaw(ctx, "diff-tree", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", commit)
	if err != nil {
		return false, err
	}
	changed := nulSet(changedBytes)
	if len(changed) != len(selected) {
		return false, nil
	}
	for path := range changed {
		if !selected[path] {
			return false, nil
		}
	}
	return true, nil
}

func (r *repo) rebasePreservingIndex(ctx context.Context, branch string) error {
	gitDir, e := r.git(ctx, "rev-parse", "--absolute-git-dir")
	if e != nil {
		return e
	}
	status, e := r.git(ctx, "status", "--porcelain", "--untracked-files=no")
	if e != nil {
		return e
	}
	var stash string
	if status != "" {
		if _, e = r.git(ctx, "stash", "push", "--message", "amux submit-work rebase"); e != nil {
			return e
		}
		stash, e = r.git(ctx, "rev-parse", "refs/stash")
		if e != nil {
			return e
		}
	}
	_, rebErr := r.git(ctx, "rebase", "--no-autostash", "@{upstream}")
	if rebErr != nil && (exists(filepath.Join(gitDir, "rebase-merge")) || exists(filepath.Join(gitDir, "rebase-apply"))) {
		if _, e = r.git(ctx, "rebase", "--abort"); e != nil {
			return fmt.Errorf("could not abort rebase; saved work remains in stash %s: %w", stash, e)
		}
	}
	if stash != "" {
		if _, e = r.git(ctx, "stash", "apply", "--index", stash); e != nil {
			return fmt.Errorf("could not restore staged and unstaged work; it remains saved in stash %s: %w", stash, e)
		}
		top, e := r.git(ctx, "rev-parse", "refs/stash")
		if e == nil && top == stash {
			if _, e = r.git(ctx, "stash", "drop", "stash@{0}"); e != nil {
				return e
			}
		}
	}
	if rebErr != nil {
		return fmt.Errorf("%s moved and rebasing onto it failed, so any active rebase was aborted and staged and unstaged work restored. If you have unrelated drafts, save them with git stash push --include-untracked and record that stash's ID with git rev-parse refs/stash. Run git pull --rebase; resolve conflicts, git add the resolved files, and git -c core.editor=true rebase --continue until finished. Restore saved drafts with git stash apply --index <saved-stash-id>; verify the staged and unstaged drafts before dropping only that saved stash. Use git rev-parse HEAD to obtain the recovered commit, then retry amux submit-work --resume-commit with that commit, the same task and file list: %w", branch, rebErr)
	}
	return nil
}
func exists(p string) bool { _, e := os.Stat(p); return e == nil }

func (r *repo) buildAndPushPR(ctx context.Context, u upstream, files []string, message, branch string) (PullRequest, error) {
	if _, e := r.git(ctx, "fetch", u.remote); e != nil {
		return PullRequest{}, e
	}
	base, e := r.git(ctx, "rev-parse", "--verify", "@{upstream}^{commit}")
	if e != nil {
		return PullRequest{}, e
	}
	diff, e := r.gitRaw(ctx, "diff", "--name-only", "--relative", "--no-renames", "-z", "HEAD", base)
	if e != nil {
		return PullRequest{}, e
	}
	selected := map[string]bool{}
	for _, p := range files {
		selected[p] = true
	}
	for _, p := range nulList(diff) {
		if selected[p] {
			return PullRequest{}, fmt.Errorf("selected paths differ between HEAD and %s: %s. Reconcile these paths with upstream before submitting; no task branch was pushed and your checkout was not changed", u.branch(), p)
		}
	}
	tmp, e := os.MkdirTemp("", "amux-index-")
	if e != nil {
		return PullRequest{}, fmt.Errorf("cannot create a temporary index: %w", e)
	}
	defer os.RemoveAll(tmp)
	stage := &repo{root: r.root, index: filepath.Join(tmp, "index")}
	if _, e = stage.git(ctx, "read-tree", base); e != nil {
		return PullRequest{}, e
	}
	if _, e = stage.gitPaths(ctx, []string{"add", "--all"}, files); e != nil {
		return PullRequest{}, e
	}
	tree, e := stage.git(ctx, "write-tree")
	if e != nil {
		return PullRequest{}, e
	}
	baseTree, e := r.git(ctx, "rev-parse", base+"^{tree}")
	if e != nil {
		return PullRequest{}, e
	}
	if tree == baseTree {
		return PullRequest{}, fmt.Errorf("the listed paths already match %s; nothing to submit", u.branch())
	}
	commit, e := r.git(ctx, "commit-tree", tree, "-p", base, "-m", message)
	if e != nil {
		return PullRequest{}, e
	}
	return r.pushPR(ctx, u, commit, message, branch)
}
func (r *repo) pushPR(ctx context.Context, u upstream, commit, message, branch string) (PullRequest, error) {
	if _, e := r.git(ctx, "push", u.remote, commit+":refs/heads/"+branch); e != nil {
		return PullRequest{}, e
	}
	title := strings.SplitN(message, "\n", 2)[0]
	return PullRequest{Root: r.root, Head: branch, Base: u.branch(), Title: title, Body: message}, nil
}
