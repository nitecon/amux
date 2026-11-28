package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

type ProjectMessageListQuery struct {
	State   string
	Kinds   []string
	AgentID *string
	Limit   *uint64
	Offset  uint64
}

// ListProjectMessageThreads reads one page of project message roots and
// project-wide counts. A nil limit leaves the server's default in effect;
// limit=0 is forwarded so storage can apply its explicit-zero default.
func (c *Client) ListProjectMessageThreads(ctx context.Context, project string, query ProjectMessageListQuery) (*storage.ProjectMessageThreadList, error) {
	project = projectid.NormalizeIdent(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	values := make(url.Values)
	if query.State != "" {
		values.Set("state", query.State)
	}
	if len(query.Kinds) != 0 {
		values.Set("kind", strings.Join(query.Kinds, ","))
	}
	if query.AgentID != nil {
		values.Set("agent_id", *query.AgentID)
	}
	if query.Limit != nil {
		values.Set("limit", strconv.FormatUint(*query.Limit, 10))
	}
	if query.Offset != 0 {
		values.Set("offset", strconv.FormatUint(query.Offset, 10))
	}
	path := "/v1/projects/" + url.PathEscape(project) + "/messages"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		return nil, err
	}
	result, err := decodeProjectMessageThreadList(raw, project)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func decodeProjectMessageThreadList(raw json.RawMessage, project string) (*storage.ProjectMessageThreadList, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return nil, fmt.Errorf("invalid project message list response")
	}
	threadsRaw, ok := envelope["threads"]
	if !ok || len(threadsRaw) == 0 || threadsRaw[0] != '[' || string(threadsRaw) == "null" {
		return nil, fmt.Errorf("project message list is missing its thread list")
	}
	countsRaw, ok := envelope["counts"]
	if !ok || len(countsRaw) == 0 || countsRaw[0] != '{' || string(countsRaw) == "null" {
		return nil, fmt.Errorf("project message list is missing its counts")
	}
	var result storage.ProjectMessageThreadList
	if json.Unmarshal(raw, &result) != nil || result.Threads == nil {
		return nil, fmt.Errorf("invalid project message list response")
	}
	var wireThreads []map[string]json.RawMessage
	if json.Unmarshal(threadsRaw, &wireThreads) != nil {
		return nil, fmt.Errorf("invalid project message list threads")
	}
	threadFields := []string{"id", "project_ident", "source", "author_kind", "agent_id", "hostname", "subject", "preview", "sent_at", "last_activity_at", "reply_count", "ack_count", "state", "resolved_at", "resolved_by"}
	nonNullThreadFields := []string{"id", "project_ident", "source", "author_kind", "preview", "sent_at", "last_activity_at", "reply_count", "ack_count", "state"}
	if len(wireThreads) != len(result.Threads) {
		return nil, fmt.Errorf("invalid project message list threads")
	}
	var counts map[string]json.RawMessage
	if json.Unmarshal(countsRaw, &counts) != nil {
		return nil, fmt.Errorf("invalid project message list counts")
	}
	for _, name := range []string{"human_open", "agent_open", "alerts_open", "system_open", "resolved"} {
		value, exists := counts[name]
		var count int64
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &count) != nil || count < 0 {
			return nil, fmt.Errorf("invalid project message list counts")
		}
	}
	previousActivity := int64(^uint64(0) >> 1)
	for i, thread := range result.Threads {
		for _, name := range threadFields {
			if _, exists := wireThreads[i][name]; !exists {
				return nil, fmt.Errorf("project message list thread is missing %s", name)
			}
		}
		for _, name := range nonNullThreadFields {
			if bytes.Equal(bytes.TrimSpace(wireThreads[i][name]), []byte("null")) {
				return nil, fmt.Errorf("project message list thread has null %s", name)
			}
		}
		if thread.ID <= 0 || thread.ProjectIdent != project || thread.LastActivityAt > previousActivity || thread.ReplyCount < 0 || thread.AckCount < 0 {
			return nil, fmt.Errorf("invalid project message list thread")
		}
		switch thread.State {
		case "open", "acknowledged", "answered", "resolved":
		default:
			return nil, fmt.Errorf("invalid project message list thread state")
		}
		previousActivity = thread.LastActivityAt
	}
	return &result, nil
}
