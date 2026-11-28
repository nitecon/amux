package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/taskplatform"
)

// TaskRouting selects how newly created work is exposed to agents. A session
// route carries the exact registered execution identity from the task-targets
// directory.
type TaskRouting struct {
	Mode   string                    `json:"mode"`
	Target *RegisteredTargetIdentity `json:"target,omitempty"`
}

func taskCreationRouting(ctx context.Context, tx queryer, project *Project, canonicalRemote *string, labels []string, requested *TaskRouting) (json.RawMessage, *agent.Origin, error) {
	routing := TaskRouting{Mode: "auto"}
	if requested != nil {
		routing = *requested
	}
	var owner *agent.Origin
	switch routing.Mode {
	case "auto", "unassigned", "broadcast":
		if routing.Target != nil {
			return nil, nil, fmt.Errorf("%w: target is only valid for session routing", ErrTaskMetadataInput)
		}
	case "session":
		if routing.Target == nil {
			return nil, nil, fmt.Errorf("%w: session routing requires a target", ErrTaskMetadataInput)
		}
		validated, err := validateRegisteredTaskTarget(ctx, tx, project.Ident, routing.Target, nil, time.Now())
		if err != nil {
			return nil, nil, err
		}
		requiredOS, err := taskplatform.RequiredOS(labels)
		if err != nil {
			return nil, nil, err
		}
		if requiredOS != "" && requiredOS != validated.OS {
			return nil, nil, fmt.Errorf("%w: routing target OS does not match task labels", ErrTaskConflict)
		}
		owner = validated
	default:
		return nil, nil, fmt.Errorf("%w: unsupported task routing mode %q", ErrTaskMetadataInput, routing.Mode)
	}
	if routing.Mode != "auto" && canonicalRemote == nil && (project.Kind != "adhoc" || project.CanonicalRemote != nil) {
		return nil, nil, ErrTargetBinding
	}
	if canonicalRemote != nil {
		if _, err := validateTargetProjectBinding(project, canonicalRemote); err != nil {
			return nil, nil, err
		}
	}
	encoded, err := json.Marshal(routing)
	if err != nil {
		return nil, nil, err
	}
	return encoded, owner, nil
}
