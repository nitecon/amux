// Package projectidentity maintains stable checkout and provider-session IDs.
package projectidentity

import (
	"bytes"
	"context"
	"crypto/sha1" // UUID v5 compatibility, not authentication or password hashing.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

// Identity contains the durable checkout identity shared by clients and workers.
type Identity struct {
	ID             string `json:"id"`
	Directory      string `json:"directory"`
	Upstream       string `json:"upstream,omitempty"`
	GatewayProject string `json:"gateway_project,omitempty"`
}

func uuidV5(namespace [16]byte, name []byte) string {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write(name)
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:])
}

func projectID(directory, upstream string) string {
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]string{"amux-project-v1", upstream, directory})
	// The standard URL namespace from RFC 4122, used by the Rust implementation.
	namespace := [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	// encoding/json always escapes U+2028/U+2029 for JavaScript safety;
	// serde_json emits them literally. Preserve escaped backslashes while
	// matching that byte representation for existing checkout identities.
	raw := bytes.TrimSuffix(input.Bytes(), []byte("\n"))
	canonical := make([]byte, 0, len(raw))
	for n := 0; n < len(raw); n++ {
		if raw[n] == '\\' && n+1 < len(raw) {
			if n+6 <= len(raw) && (string(raw[n:n+6]) == `\u2028` || string(raw[n:n+6]) == `\u2029`) {
				if raw[n+5] == '8' {
					canonical = append(canonical, "\u2028"...)
				} else {
					canonical = append(canonical, "\u2029"...)
				}
				n += 5
				continue
			}
			canonical = append(canonical, raw[n], raw[n+1])
			n++
			continue
		}
		canonical = append(canonical, raw[n])
	}
	return uuidV5(namespace, canonical)
}

// SessionID deterministically namespaces an agent name under its checkout.
func (i Identity) SessionID(agentName string) string {
	var namespace [16]byte
	decoded, err := hex.DecodeString(strings.ReplaceAll(i.ID, "-", ""))
	if err != nil || len(decoded) != 16 {
		return ""
	}
	copy(namespace[:], decoded)
	return uuidV5(namespace, []byte(agentName))
}

// FromParts canonicalizes filesystem aliases and repository URLs before hashing.
func FromParts(directory, upstream string) (Identity, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve checkout directory: %w", err)
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve checkout directory: %w", err)
	}
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(directory, `\\?\UNC\`) {
			directory = `\\` + strings.TrimPrefix(directory, `\\?\UNC\`)
		} else {
			directory = strings.TrimPrefix(directory, `\\?\`)
		}
	}
	info, err := os.Stat(directory)
	if err != nil {
		return Identity{}, err
	}
	if !info.IsDir() {
		return Identity{}, fmt.Errorf("project directory must be a directory")
	}
	project := ""
	if upstream != "" {
		upstream, err = projects.CanonicalRemote(upstream)
		if err != nil {
			return Identity{}, err
		}
		project = upstream[strings.LastIndexByte(upstream, '/')+1:]
		if project == "." || project == ".." || project == "" {
			return Identity{}, fmt.Errorf("invalid gateway project identifier")
		}
		for _, r := range project {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.", r)) {
				return Identity{}, fmt.Errorf("invalid gateway project identifier")
			}
		}
	}
	return Identity{ID: projectID(directory, upstream), Directory: directory, Upstream: upstream, GatewayProject: project}, nil
}

// Resolve uses the checkout root when Git can resolve it. As in Rust, absent or
// unsupported upstreams produce a stable local identity, not a guessed binding.
func Resolve(ctx context.Context, directory string) (Identity, error) {
	local, err := FromParts(directory, "")
	if err != nil {
		return Identity{}, err
	}
	root, err := observe(ctx, local.Directory, "rev-parse", "--show-toplevel")
	if err == nil {
		root = strings.TrimRight(root, "\r\n")
		if len(root) <= 4096 && filepath.IsAbs(root) {
			local, err = FromParts(root, "")
			if err != nil {
				return Identity{}, err
			}
		}
	}
	upstream, err := observe(ctx, local.Directory, "config", "--local", "--get", "remote.origin.url")
	if ctx.Err() != nil {
		return Identity{}, ctx.Err()
	}
	if err != nil {
		return local, nil
	}
	upstream = strings.TrimRight(upstream, "\r\n")
	canonical, err := projects.CanonicalRemote(upstream)
	if err != nil {
		return local, nil
	}
	return FromParts(local.Directory, canonical)
}

// ResolveCheckout requires directory to belong to a Git checkout. Unlike
// Resolve, it does not fall back to a local-only identity when Git is absent.
func ResolveCheckout(ctx context.Context, directory string) (Identity, error) {
	local, err := FromParts(directory, "")
	if err != nil {
		return Identity{}, err
	}
	root, err := observe(ctx, local.Directory, "rev-parse", "--show-toplevel")
	if err != nil {
		return Identity{}, fmt.Errorf("directory is not an available Git checkout")
	}
	root = strings.TrimRight(root, "\r\n")
	if root == "" || !filepath.IsAbs(root) || len(root) > 4096 {
		return Identity{}, fmt.Errorf("Git checkout root is invalid")
	}
	return Resolve(ctx, root)
}

// boundedOutput drains command stdout while retaining only the accepted limit.
// Draining avoids the pipe-capacity deadlock in the original Rust observer.
type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remain := 4099 - len(b.data); remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func observe(ctx context.Context, directory string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	base := []string{"--no-optional-locks", "-c", "core.hooksPath=" + os.DevNull, "-C", directory}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cannot observe Git checkout")
	}
	if len(out.data) > 4098 {
		return "", fmt.Errorf("Git checkout response exceeds limit")
	}
	return string(out.data), nil
}
