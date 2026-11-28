package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

type TaskRouting = storage.TaskRouting

const (
	TaskRoutingAuto       = "auto"
	TaskRoutingUnassigned = "unassigned"
	TaskRoutingBroadcast  = "broadcast"
	TaskRoutingSession    = "session"
)

// CreateRoutedTask creates a human-authored task with source-compatible routing
// and validates the gateway receipt. remote is required for repository-backed
// routing and may be nil only for auto routing or an ad hoc local project.
func (c *Client) CreateRoutedTask(ctx context.Context, project string, input TaskCreateInput, remote *string, routing TaskRouting) (*storage.Task, error) {
	input.CanonicalRemote = remote
	input.Routing = &routing
	return c.CreateTask(ctx, project, input)
}

func (c *Client) validateCreateRouting(ctx context.Context, project string, input *TaskCreateInput) error {
	project = projectid.NormalizeIdent(project)
	routing := input.Routing
	if routing.Mode == "" {
		routing.Mode = TaskRoutingAuto
	}
	switch routing.Mode {
	case TaskRoutingAuto, TaskRoutingUnassigned, TaskRoutingBroadcast:
		if routing.Target != nil {
			return fmt.Errorf("%s task routing must not include a session target", routing.Mode)
		}
	case TaskRoutingSession:
		if routing.Target == nil || validateRequesterTarget(routing.Target, nil) != nil {
			return fmt.Errorf("session task routing requires a complete canonical target")
		}
	default:
		return fmt.Errorf("unsupported task routing mode")
	}
	if input.CanonicalRemote == nil && routing.Mode == TaskRoutingAuto {
		return nil
	}
	requestedRemote := ""
	if input.CanonicalRemote != nil {
		remote, err := projectid.CanonicalRemote(*input.CanonicalRemote)
		if err != nil {
			return fmt.Errorf("invalid task repository")
		}
		requestedRemote = remote
	}
	listed, err := c.Projects(ctx)
	if err != nil {
		return err
	}
	if len(listed) > 4096 {
		return fmt.Errorf("gateway returned too many projects")
	}
	active := make(map[string]storage.Project, len(listed))
	remoteMatches := 0
	remoteProject := ""
	for _, candidate := range listed {
		if candidate.ArchivedAt != nil {
			continue
		}
		if strings.TrimSpace(candidate.Ident) == "" || len(candidate.Ident) > 512 || strings.IndexFunc(candidate.Ident, unicode.IsControl) >= 0 {
			return fmt.Errorf("gateway project list contains an invalid project identity")
		}
		if _, exists := active[candidate.Ident]; exists {
			return fmt.Errorf("gateway project list contains a duplicate project identity")
		}
		active[candidate.Ident] = candidate.Project
		if candidate.CanonicalRemote == nil {
			continue
		}
		remote, normalizeErr := projectid.CanonicalRemote(*candidate.CanonicalRemote)
		if normalizeErr != nil || remote != requestedRemote {
			continue
		}
		remoteMatches++
		remoteProject = candidate.Ident
	}
	if input.CanonicalRemote != nil {
		if remoteMatches > 1 {
			return fmt.Errorf("gateway projects contain multiple active mappings for the requested repository")
		}
		if remoteMatches != 1 || projectid.NormalizeIdent(remoteProject) != project {
			return fmt.Errorf("repository project changed; refresh before submitting")
		}
		input.CanonicalRemote = &requestedRemote
	} else {
		candidate, exists := active[project]
		if !exists || candidate.Kind != "adhoc" || candidate.CanonicalRemote != nil {
			return fmt.Errorf("explicit task routing requires a repository project or an ad hoc local project")
		}
	}
	return nil
}

func routedTaskAcknowledgement(task *storage.Task, requested TaskRouting) error {
	if requested.Mode == "" {
		requested.Mode = TaskRoutingAuto
	}
	var acknowledged TaskRouting
	if err := json.Unmarshal(task.Routing, &acknowledged); err != nil || acknowledged.Mode != requested.Mode {
		return fmt.Errorf("task routing unconfirmed; inspect the task before retrying")
	}
	switch requested.Mode {
	case TaskRoutingAuto, TaskRoutingUnassigned, TaskRoutingBroadcast:
		if acknowledged.Target != nil || task.OwnerOrigin != nil {
			return fmt.Errorf("task routing or owner unconfirmed; inspect the task before retrying")
		}
	case TaskRoutingSession:
		if requested.Target == nil || acknowledged.Target == nil || *acknowledged.Target != *requested.Target || task.OwnerOrigin == nil || *task.OwnerOrigin != requested.Target.Origin {
			return fmt.Errorf("task recipient unconfirmed; inspect the task before retrying")
		}
	default:
		return fmt.Errorf("task routing unconfirmed; inspect the task before retrying")
	}
	return nil
}
