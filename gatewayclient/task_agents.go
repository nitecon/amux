package gatewayclient

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

// TaskAgents returns the live targets for a checkout whose canonical remote
// matches the gateway project. Discovery is read-only and makes one project
// lookup and one target-directory lookup.
func (c *Client) TaskAgents(ctx context.Context, project, checkoutRemote, requiredOS string) (storage.TargetDirectory, error) {
	if requiredOS != "" && requiredOS != "linux" && requiredOS != "windows" && requiredOS != "macos" {
		return storage.TargetDirectory{}, fmt.Errorf("unsupported OS %q: use linux, windows or macos", requiredOS)
	}
	remote, err := projects.CanonicalRemote(checkoutRemote)
	if err != nil {
		return storage.TargetDirectory{}, fmt.Errorf("current checkout has no canonical repository binding")
	}
	project = projects.NormalizeIdent(project)
	listed, err := c.Projects(ctx)
	if err != nil {
		return storage.TargetDirectory{}, err
	}
	var binding *string
	for i := range listed {
		if listed[i].Ident == project {
			binding = listed[i].CanonicalRemote
			break
		}
	}
	if binding == nil || *binding != remote {
		return storage.TargetDirectory{}, fmt.Errorf("current checkout is not bound to gateway project %q", project)
	}
	directory, err := c.taskTargetDirectory(ctx, project)
	if err != nil {
		return storage.TargetDirectory{}, err
	}
	if directory.CanonicalRemote == nil || *directory.CanonicalRemote != remote {
		return storage.TargetDirectory{}, fmt.Errorf("task target directory does not match the current checkout repository")
	}
	directory.Targets, err = selectLiveTaskTargets(directory, requiredOS, time.Now().UnixMilli())
	if err != nil {
		return storage.TargetDirectory{}, err
	}
	return directory, nil
}

// selectLiveTaskTargets is shared by read-only discovery and reassignment so
// both use the same origin, generation, lease, expiry and OS rules.
func selectLiveTaskTargets(directory storage.TargetDirectory, requiredOS string, now int64) ([]storage.RegisteredTarget, error) {
	if requiredOS != "" && requiredOS != "linux" && requiredOS != "windows" && requiredOS != "macos" {
		return nil, fmt.Errorf("unsupported OS %q: use linux, windows or macos", requiredOS)
	}
	selected := make([]storage.RegisteredTarget, 0, len(directory.Targets))
	for _, entry := range directory.Targets {
		generation, generationErr := uuid.Parse(entry.Target.Generation)
		origin, originErr := storage.NormalizeOrigin(&entry.Target.Origin)
		if generationErr != nil || generation == uuid.Nil || generation.String() != entry.Target.Generation || originErr != nil || origin == nil || *origin != entry.Target.Origin || entry.SeenAt <= 0 || entry.ExpiresAt <= entry.SeenAt || entry.ExpiresAt-entry.SeenAt > 120000 {
			return nil, fmt.Errorf("invalid gateway target directory response")
		}
		if entry.ExpiresAt <= now || (requiredOS != "" && entry.Target.Origin.OS != requiredOS) {
			continue
		}
		selected = append(selected, entry)
	}
	return selected, nil
}
