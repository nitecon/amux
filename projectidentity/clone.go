package projectidentity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

const cloneMarker = ".git/amux-clone-complete"

// CloneRepository clones repository below parent and reuses only a verified
// checkout. It never removes or overwrites an existing directory.
func CloneRepository(ctx context.Context, parent, repository string) (string, error) {
	destination, err := CloneDestination(parent, repository)
	if err != nil {
		return "", err
	}
	return cloneRepositoryAt(ctx, filepath.Dir(destination), filepath.Base(destination), strings.TrimSpace(repository))
}

// CloneDestination returns the folder CloneRepository uses beneath parent.
func CloneDestination(parent, repository string) (string, error) {
	name, err := cloneName(strings.TrimSpace(repository))
	if err != nil {
		return "", err
	}
	parent, err = canonicalDirectory(parent)
	if err != nil {
		return "", fmt.Errorf("clone parent is not an available directory")
	}
	return filepath.Join(parent, name), nil
}

// CloneRepositoryAt clones into the exact destination, which may be an empty
// directory or a new folder under an existing parent.
func CloneRepositoryAt(ctx context.Context, destination, repository string) (string, error) {
	destination = strings.TrimSpace(destination)
	if !filepath.IsAbs(destination) {
		return "", fmt.Errorf("enter an absolute clone destination")
	}
	destination = filepath.Clean(destination)
	parent, name := filepath.Dir(destination), filepath.Base(destination)
	if parent == destination || name == "." || strings.ContainsAny(name, "?#%\"<>|*") {
		return "", fmt.Errorf("clone needs a usable destination folder")
	}
	return cloneRepositoryAt(ctx, parent, name, strings.TrimSpace(repository))
}

func cloneRepositoryAt(ctx context.Context, parent, name, repository string) (string, error) {
	if repository == "" {
		return "", fmt.Errorf("enter a repository URL or Git path")
	}
	if err := validateCloneSource(repository); err != nil {
		return "", err
	}
	parent, err := canonicalDirectory(parent)
	if err != nil {
		return "", fmt.Errorf("clone parent is not an available directory")
	}
	destination := filepath.Join(parent, name)
	reservation := filepath.Join(parent, "."+name+".amux-clone-incomplete")
	lock, err := openCloneReservation(reservation)
	if err != nil {
		return "", fmt.Errorf("reserve clone destination: %w", err)
	}
	defer lock.Close()
	if err := tryCloneLock(lock); err != nil {
		return "", fmt.Errorf("clone is already running at %s", destination)
	}

	state, err := io.ReadAll(io.LimitReader(lock, 64))
	if err != nil {
		return "", fmt.Errorf("read clone reservation: %w", err)
	}
	if strings.TrimSpace(string(state)) == "incomplete" {
		if _, err := os.Lstat(destination); err == nil {
			return "", fmt.Errorf("%s is incomplete; inspect or remove it before retrying", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect clone destination: %w", err)
		}
		// Missing destination means the user explicitly removed the failed clone.
	}

	useExistingEmptyDirectory := false
	if info, err := os.Lstat(destination); err == nil {
		if info.IsDir() {
			entries, readErr := os.ReadDir(destination)
			if readErr != nil {
				return "", fmt.Errorf("inspect clone destination: %w", readErr)
			}
			useExistingEmptyDirectory = len(entries) == 0 && len(state) == 0
		}
		if !useExistingEmptyDirectory {
			return reusableCheckout(ctx, destination, repository, parent)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect clone destination: %w", err)
	}
	if err := writeCloneState(lock, "incomplete\n"); err != nil {
		return "", fmt.Errorf("mark clone in progress: %w", err)
	}
	if !useExistingEmptyDirectory {
		if err := os.Mkdir(destination, 0755); err != nil {
			return "", fmt.Errorf("create clone destination: %w", err)
		}
	}

	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cloneCtx, "git", "clone", "--", repository, destination)
	cmd.Dir = parent
	cmd.Env = cloneEnvironment()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if cloneCtx.Err() != nil {
			return "", fmt.Errorf("git clone timed out; partial folder preserved at %s", destination)
		}
		return "", fmt.Errorf("git clone failed; partial folder preserved at %s", destination)
	}
	if err := os.WriteFile(filepath.Join(destination, cloneMarker), []byte("complete\n"), 0600); err != nil {
		return "", fmt.Errorf("record clone completion; checkout preserved at %s: %w", destination, err)
	}
	if err := writeCloneState(lock, ""); err != nil {
		return "", fmt.Errorf("clear clone reservation; checkout preserved at %s: %w", destination, err)
	}
	return canonicalDirectory(destination)
}

// CloneOrigin returns the checkout's configured origin in cloneable form.
// Credentials, queries, and fragments are removed before the value can be
// shared with another machine. The clone origin is not canonicalized.
func CloneOrigin(ctx context.Context, directory string) (*string, error) {
	identity, err := ResolveCheckout(ctx, directory)
	if err != nil {
		return nil, err
	}
	origin, err := observe(ctx, identity.Directory, "config", "--local", "--get", "remote.origin.url")
	if err != nil {
		// Git exits nonzero when a checkout has no configured origin.
		return nil, nil
	}
	origin = strings.TrimRight(origin, "\r\n")
	if origin == "" {
		return nil, nil
	}
	var safe bool
	origin, safe = SanitizeCloneOriginURL(origin)
	if !safe {
		return nil, nil
	}
	if !strings.Contains(origin, "://") {
		origin = safeNonURLCloneOrigin(origin)
		if origin == "[redacted]" {
			return nil, nil
		}
	}
	return ptrString(origin), nil
}

func ptrString(value string) *string { return &value }

// SanitizeCloneOriginURL removes URL credentials and query/fragment data while
// preserving a usable clone spelling. SSH's conventional git username is part
// of the transport and is retained; other SSH user identities cannot be
// safely rewritten. Non-URL Git paths are returned unchanged for caller policy.
func SanitizeCloneOriginURL(origin string) (string, bool) {
	marker := strings.Index(origin, "://")
	if marker < 0 {
		return origin, true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return "", false
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		_, hasPassword := parsed.User.Password()
		if parsed.Scheme == "ssh" && username == "git" && !hasPassword {
			// Preserve the transport username exactly.
		} else if parsed.Scheme == "http" || parsed.Scheme == "https" {
			// HTTP credentials are optional for the source spelling and must not
			// leave this machine.
		} else {
			return "", false
		}
	}

	rest := origin[marker+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority := rest[:end]
	if parsed.User != nil && !(parsed.Scheme == "ssh" && parsed.User.Username() == "git") {
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			authority = authority[at+1:]
		}
	}
	path := rest[end:]
	if tail := strings.IndexAny(path, "?#"); tail >= 0 {
		path = path[:tail]
	}
	return origin[:marker+3] + authority + path, true
}

func safeNonURLCloneOrigin(origin string) string {
	if filepath.IsAbs(origin) || strings.HasPrefix(origin, "git@") || !strings.Contains(origin, "@") {
		return origin
	}
	if canonical, err := projects.CanonicalRemote(origin); err == nil {
		return canonical
	}
	return "[redacted]"
}

func cloneName(repository string) (string, error) {
	if repository == "" || strings.HasPrefix(repository, "-") || strings.ContainsAny(repository, "\r\n") {
		return "", fmt.Errorf("enter a repository URL or Git path")
	}
	trimmed := strings.TrimRight(repository, "/")
	name := trimmed[strings.LastIndexAny(trimmed, "/\\:")+1:]
	name = strings.TrimSuffix(name, ".git")
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "?#%\"<>|*") || filepath.Base(name) != name {
		return "", fmt.Errorf("repository needs a usable folder name")
	}
	return name, nil
}

func validateCloneSource(source string) error {
	if len(source) > 4096 || strings.IndexFunc(source, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 || strings.HasPrefix(source, "-") || strings.HasPrefix(source, "ext::") {
		return fmt.Errorf("unsupported Git repository source")
	}
	if _, err := projects.CanonicalRemote(source); err == nil {
		return nil
	}
	// Git local paths and file URLs are supported; Git's command transport and
	// unknown URL schemes are not.
	if strings.Contains(source, "://") {
		if strings.HasPrefix(source, "file://") {
			return nil
		}
		return fmt.Errorf("unsupported Git repository source")
	}
	if strings.Contains(source, "::") || strings.Contains(source, "@") && !filepath.IsAbs(source) && !strings.Contains(source, ":/") && !validSCPRemote(source) {
		return fmt.Errorf("unsupported Git repository source")
	}
	return nil
}

// ValidateCloneSource exposes the same source constraints used by CloneRepository
// to callers that need to display or transport an already observed clone origin.
func ValidateCloneSource(source string) error { return validateCloneSource(source) }

func validSCPRemote(source string) bool {
	userHost, path, ok := strings.Cut(source, ":")
	return ok && strings.Contains(userHost, "@") && path != "" && !strings.HasPrefix(path, "-") && !strings.ContainsAny(source, " \t")
}

func reusableCheckout(ctx context.Context, destination, repository, parent string) (string, error) {
	path, err := canonicalDirectory(destination)
	if err != nil {
		return "", fmt.Errorf("%s already exists and is not a verified checkout", destination)
	}
	root, err := observe(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s already exists and is not a checkout of the requested repository", destination)
	}
	root = strings.TrimRight(root, "\r\n")
	canonicalRoot, err := canonicalDirectory(root)
	if err != nil || canonicalRoot != path {
		return "", fmt.Errorf("%s is not the checkout root", destination)
	}
	if sameLocalCheckout(parent, repository, path) {
		// Rust reuses the exact selected local checkout even before its first
		// commit; root identity and the reservation are the relevant proofs here.
		return path, nil
	}
	origin, originErr := observe(ctx, path, "config", "--local", "--get", "remote.origin.url")
	if originErr != nil || !sameRepository(parent, repository, strings.TrimRight(origin, "\r\n"), path) {
		return "", fmt.Errorf("%s already exists and is not a checkout of the requested repository", destination)
	}
	return completeCheckout(ctx, path, destination)
}

func completeCheckout(ctx context.Context, path, destination string) (string, error) {
	_, headErr := observe(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	marker, markerErr := os.Stat(filepath.Join(path, filepath.FromSlash(cloneMarker)))
	if headErr != nil && (markerErr != nil || !marker.Mode().IsRegular()) {
		return "", fmt.Errorf("%s is incomplete; inspect or remove it before retrying", destination)
	}
	return path, nil
}

func sameLocalCheckout(parent, requested, checkout string) bool {
	if _, err := projects.CanonicalRemote(requested); err == nil || strings.Contains(requested, "://") {
		return false
	}
	requestedPath := requested
	if !filepath.IsAbs(requestedPath) {
		requestedPath = filepath.Join(parent, requestedPath)
	}
	resolved, err := canonicalDirectory(requestedPath)
	return err == nil && resolved == checkout
}

func sameRepository(parent, requested, origin, checkout string) bool {
	requestedRemote, requestedRemoteErr := projects.CanonicalRemote(requested)
	originRemote, originRemoteErr := projects.CanonicalRemote(origin)
	if requestedRemoteErr == nil && originRemoteErr == nil {
		return requestedRemote == originRemote
	}
	canonicalPath := func(path, base string) (string, error) {
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		return canonicalDirectory(path)
	}
	requestedPath, requestedErr := canonicalPath(requested, parent)
	originPath, originErr := canonicalPath(origin, checkout)
	return requestedErr == nil && originErr == nil && requestedPath == originPath
}

func canonicalDirectory(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return path, nil
}

func writeCloneState(file *os.File, value string) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := io.WriteString(file, value); err != nil {
		return err
	}
	return file.Sync()
}

func openCloneReservation(path string) (*os.File, error) {
	before, beforeErr := os.Lstat(path)
	if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
		return nil, beforeErr
	}
	if beforeErr == nil && !before.Mode().IsRegular() {
		return nil, fmt.Errorf("clone reservation is not a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	after, lstatErr := os.Lstat(path)
	opened, statErr := file.Stat()
	if lstatErr != nil || statErr != nil || !after.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(after, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("clone reservation changed or is not a regular file")
	}
	return file, nil
}

func cloneEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}
