package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

// GetProjectMessageThread fetches the full project-scoped thread for a root or
// reply message ID. It is a read-only request and does not require agent-origin
// headers.
func (c *Client) GetProjectMessageThread(ctx context.Context, project string, messageID int64) (*storage.ProjectMessageThread, error) {
	project = projectid.NormalizeIdent(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if messageID <= 0 {
		return nil, fmt.Errorf("message ID must be positive")
	}
	var thread storage.ProjectMessageThread
	path := "/v1/projects/" + url.PathEscape(project) + "/messages/" + strconv.FormatInt(messageID, 10) + "/thread"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &thread); err != nil {
		return nil, err
	}
	if err := validateProjectMessageThread(project, messageID, &thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

func validateProjectMessageThread(project string, requestedID int64, thread *storage.ProjectMessageThread) error {
	if thread == nil || thread.Root.ID <= 0 || thread.Root.ProjectIdent != project || thread.Replies == nil || thread.Confirmations == nil {
		return fmt.Errorf("invalid project message thread response")
	}
	switch thread.State {
	case "open", "acknowledged", "answered", "resolved":
	default:
		return fmt.Errorf("invalid project message thread state")
	}
	foundRequested := thread.Root.ID == requestedID
	previousID := int64(0)
	for _, reply := range thread.Replies {
		if reply.ID <= previousID || reply.ProjectIdent != project || reply.ParentMessageID == nil || *reply.ParentMessageID != thread.Root.ID {
			return fmt.Errorf("invalid project message thread replies")
		}
		if reply.ID == requestedID {
			foundRequested = true
		}
		previousID = reply.ID
	}
	if !foundRequested {
		return fmt.Errorf("project message thread does not contain the requested message")
	}
	return nil
}
