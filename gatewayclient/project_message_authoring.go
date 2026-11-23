package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/nitecon/amux/projects"
)

type HumanProjectMessageInput struct {
	Content         string `json:"content"`
	ParentMessageID *int64 `json:"parent_message_id,omitempty"`
	Author          string `json:"author,omitempty"`
	Resolve         bool   `json:"resolve,omitempty"`
}

type HumanProjectMessageReceipt struct {
	MessageID       int64
	ParentMessageID *int64
	Delivered       bool
	DeliveryError   *string
}

// PostHumanProjectMessage stores a human-authored project message. Reply
// requests first resolve their parent to the thread root so the subsequent
// receipt is checked against the exact parent the server should commit.
func (c *Client) PostHumanProjectMessage(ctx context.Context, project string, input HumanProjectMessageInput) (*HumanProjectMessageReceipt, error) {
	project = projects.NormalizeIdent(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	input.Content = strings.TrimSpace(input.Content)
	if input.Content == "" {
		return nil, fmt.Errorf("message content must not be empty")
	}
	input.Author = strings.TrimSpace(input.Author)
	var expectedParent *int64
	if input.ParentMessageID == nil || *input.ParentMessageID <= 0 {
		input.ParentMessageID = nil // source treats invalid/non-positive parent values as a root message
	} else {
		requestedParentID := *input.ParentMessageID
		thread, err := c.GetProjectMessageThread(ctx, project, requestedParentID)
		if err != nil {
			return nil, fmt.Errorf("resolve reply thread before posting: %w", err)
		}
		if thread == nil || thread.Root.ID <= 0 || thread.Root.ProjectIdent != project || thread.Root.ID > requestedParentID {
			return nil, fmt.Errorf("gateway returned an invalid project message thread; message was not submitted")
		}
		if thread.Root.ID != requestedParentID {
			// Keep the original message ID in the write. Source POST follows
			// exactly one parent link, so a nested reply's acknowledged parent
			// is the thread endpoint's returned root even when it has a parent.
			rootID := thread.Root.ID
			expectedParent = &rootID
		} else if thread.Root.ParentMessageID == nil || *thread.Root.ParentMessageID == requestedParentID {
			// A normal root or legacy self-parent follows to itself.
			rootID := thread.Root.ID
			expectedParent = &rootID
		} else {
			// The requested reply survives but its one-hop parent does not.
			// Source POST therefore creates a new root; retain the original ID
			// and pin the expected null parent in the receipt.
		}
	}
	var raw json.RawMessage
	path := "/v1/projects/" + url.PathEscape(project) + "/messages/human"
	if err := c.do(ctx, http.MethodPost, path, input, nil, &raw); err != nil {
		return nil, fmt.Errorf("human message outcome is uncertain; inspect unread messages before retrying: %w", err)
	}
	receipt, err := parseHumanProjectMessageReceipt(raw, expectedParent)
	if err != nil {
		return nil, fmt.Errorf("human message outcome is uncertain; inspect unread messages before retrying: %w", err)
	}
	return receipt, nil
}

func parseHumanProjectMessageReceipt(raw json.RawMessage, expectedParent *int64) (*HumanProjectMessageReceipt, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("invalid human message acknowledgement")
	}
	for _, name := range []string{"message_id", "parent_message_id", "delivered", "delivery_error"} {
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("human message acknowledgement omitted %s", name)
		}
	}
	var decoded struct {
		MessageID int64 `json:"message_id"`
		Delivered *bool `json:"delivered"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.MessageID <= 0 || decoded.Delivered == nil {
		return nil, fmt.Errorf("invalid human message acknowledgement")
	}
	if expectedParent != nil && decoded.MessageID <= *expectedParent {
		return nil, fmt.Errorf("gateway acknowledged a message ID that does not follow its parent")
	}
	parentRaw := bytes.TrimSpace(fields["parent_message_id"])
	var parentID *int64
	if !bytes.Equal(parentRaw, []byte("null")) {
		var value int64
		if len(parentRaw) == 0 || json.Unmarshal(parentRaw, &value) != nil || value <= 0 {
			return nil, fmt.Errorf("invalid acknowledged parent message ID")
		}
		parentID = &value
	}
	if !sameMessageID(parentID, expectedParent) {
		return nil, fmt.Errorf("gateway acknowledged a different project message thread")
	}
	errorRaw := bytes.TrimSpace(fields["delivery_error"])
	var deliveryError *string
	if !bytes.Equal(errorRaw, []byte("null")) {
		var value string
		if len(errorRaw) == 0 || json.Unmarshal(errorRaw, &value) != nil || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("invalid channel delivery error in human message acknowledgement")
		}
		deliveryError = &value
	}
	if *decoded.Delivered == (deliveryError != nil) {
		return nil, fmt.Errorf("human message delivery status and error disagree")
	}
	return &HumanProjectMessageReceipt{MessageID: decoded.MessageID, ParentMessageID: parentID, Delivered: *decoded.Delivered, DeliveryError: deliveryError}, nil
}

func sameMessageID(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
