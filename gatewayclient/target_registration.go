package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/nitecon/amux/storage"
)

type targetRegistrationBody struct {
	Generation      string  `json:"generation"`
	CanonicalRemote *string `json:"canonical_remote"`
	DisplayName     *string `json:"display_name"`
	Hostname        *string `json:"hostname"`
}

func taskTargetsPath(project string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/task-targets"
}

// RegisterTarget explicitly advertises the caller's native target for manual
// routing. The origin is sent only in the standard native-origin headers.
func (c *Client) RegisterTarget(ctx context.Context, project string, request storage.TargetRegistration) (storage.RegisteredTarget, error) {
	origin, err := storage.NormalizeOrigin(&request.Origin)
	if err != nil || origin == nil {
		return storage.RegisteredTarget{}, fmt.Errorf("target registration requires a complete native agent origin")
	}
	body := targetRegistrationBody{
		Generation: request.Generation, CanonicalRemote: request.CanonicalRemote,
		DisplayName: request.DisplayName, Hostname: request.Hostname,
	}
	var registered storage.RegisteredTarget
	if err := c.do(ctx, http.MethodPost, taskTargetsPath(project), body, origin, &registered); err != nil {
		return storage.RegisteredTarget{}, err
	}
	if registered.Target.Generation == "" || registered.Target.Origin != *origin || registered.SeenAt <= 0 || registered.ExpiresAt <= registered.SeenAt {
		return storage.RegisteredTarget{}, fmt.Errorf("invalid gateway target registration response")
	}
	return registered, nil
}

// Targets lists the live, explicitly registered targets for a project.
func (c *Client) Targets(ctx context.Context, project string) ([]storage.RegisteredTarget, error) {
	directory, err := c.taskTargetDirectory(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, target := range directory.Targets {
		if target.Target.Generation == "" || target.SeenAt <= 0 || target.ExpiresAt <= target.SeenAt {
			return nil, fmt.Errorf("invalid gateway target directory response")
		}
	}
	return directory.Targets, nil
}
