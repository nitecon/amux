package project

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LocalCheckout describes an immediate checkout below a configured projects
// directory. CanonicalRemote is nil when the checkout has no supported origin.
type LocalCheckout struct {
	Directory       string
	CanonicalRemote *string
}

// DiscoverLocalCheckouts lists only immediate child directories that contain
// a Git metadata entry. It observes each checkout through Resolve and does not
// register or modify
func DiscoverLocalCheckouts(ctx context.Context, parent string) []LocalCheckout {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return nil
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return nil
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}

	checkouts := make([]LocalCheckout, 0)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil
		}
		path := filepath.Join(parent, entry.Name())
		childInfo, err := os.Stat(path)
		if err != nil || !childInfo.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			continue
		}
		identity, err := Resolve(ctx, path)
		if err != nil {
			continue
		}
		var remote *string
		if identity.Upstream != "" {
			remote = &identity.Upstream
		}
		checkouts = append(checkouts, LocalCheckout{Directory: identity.Directory, CanonicalRemote: remote})
	}
	sort.Slice(checkouts, func(i, j int) bool { return checkouts[i].Directory < checkouts[j].Directory })
	checkouts = compactLocalCheckouts(checkouts)
	return checkouts
}

func compactLocalCheckouts(checkouts []LocalCheckout) []LocalCheckout {
	if len(checkouts) < 2 {
		return checkouts
	}
	out := checkouts[:1]
	for _, checkout := range checkouts[1:] {
		if checkout.Directory != out[len(out)-1].Directory {
			out = append(out, checkout)
		}
	}
	return out
}
