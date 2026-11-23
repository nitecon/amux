package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

type UnreadProjectMessagesResponse struct {
	Messages []storage.ProjectMessage `json:"messages"`
	Status   string                   `json:"status"`
}

func (c *Client) UnreadProjectMessages(ctx context.Context, project string, origin storage.AgentOrigin) (*UnreadProjectMessagesResponse, error) {
	project = projects.NormalizeIdent(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	actor, err := storage.NormalizeOrigin(&origin)
	if err != nil {
		return nil, err
	}
	var response UnreadProjectMessagesResponse
	path := "/v1/projects/" + url.PathEscape(project) + "/messages/unread"
	if err := c.do(ctx, http.MethodGet, path, nil, actor, &response); err != nil {
		return nil, err
	}
	if response.Messages == nil {
		return nil, fmt.Errorf("unread messages response is missing its message list")
	}
	wantStatus := "no messages"
	if len(response.Messages) != 0 {
		wantStatus = strconv.Itoa(len(response.Messages)) + " unread user message(s)"
	}
	if response.Status != wantStatus {
		return nil, fmt.Errorf("unread messages response status does not match its message list")
	}
	previousID := int64(0)
	for _, message := range response.Messages {
		if message.ID <= previousID || message.ProjectIdent != project {
			return nil, fmt.Errorf("unread message response has an invalid project or message ID")
		}
		previousID = message.ID
	}
	return &response, nil
}

// ConfirmProjectMessage acknowledges one message for this exact native
// session. A lost or malformed response is uncertain; inspect unread messages
// before deciding whether to issue another confirmation.
func (c *Client) ConfirmProjectMessage(ctx context.Context, project string, messageID int64, origin storage.AgentOrigin) (bool, error) {
	project = projects.NormalizeIdent(project)
	if project == "" {
		return false, fmt.Errorf("project is required")
	}
	if messageID <= 0 {
		return false, fmt.Errorf("message ID must be positive")
	}
	actor, err := storage.NormalizeOrigin(&origin)
	if err != nil {
		return false, err
	}
	var response struct {
		Confirmed *bool `json:"confirmed"`
	}
	path := "/v1/projects/" + url.PathEscape(project) + "/messages/" + strconv.FormatInt(messageID, 10) + "/confirm"
	if err := c.do(ctx, http.MethodPost, path, nil, actor, &response); err != nil {
		return false, fmt.Errorf("message confirmation not confirmed; inspect unread messages before retrying: %w", err)
	}
	if response.Confirmed == nil {
		return false, fmt.Errorf("message confirmation response is uncertain; inspect unread messages before retrying")
	}
	return *response.Confirmed, nil
}
