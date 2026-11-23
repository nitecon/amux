package codex

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providerrpc"
)

func (s *Session) handle(ctx context.Context, p *providerrpc.Peer, message providerrpc.Message) error {
	if message.ID != nil {
		return s.request(ctx, p, message)
	}
	if !knownNotification(message.Method) {
		return nil
	}
	var params struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		ItemID    string          `json:"itemId"`
		Delta     string          `json:"delta"`
		RequestID json.RawMessage `json:"requestId"`
		Thread    struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn struct{ ID, Status string }                                  `json:"turn"`
		Item struct{ ID, Type, Text, Command, Tool, Name, Status string } `json:"item"`
	}
	if json.Unmarshal(message.Params, &params) != nil {
		return fmt.Errorf("invalid Codex notification")
	}
	if message.Method == "thread/started" {
		if params.Thread.ID == "" {
			return fmt.Errorf("Codex thread notification has no ID")
		}
		s.mu.Lock()
		if s.id == "" {
			s.id = params.Thread.ID
		}
		owned := s.id == params.Thread.ID
		s.mu.Unlock()
		if !owned {
			return nil
		}
		return s.emit(ctx, conversation.Event{Kind: "state", SessionID: params.Thread.ID, Status: "thread_started", Data: message.Params})
	}
	if params.ThreadID == "" {
		return fmt.Errorf("Codex notification %q has no thread ID", message.Method)
	}
	thread := s.ID()
	// Account/global notifications are not chat content. Foreign thread output
	// never becomes text in this task's conversation.
	if params.ThreadID == "" || params.ThreadID != thread {
		return nil
	}
	if message.Method == "serverRequest/resolved" {
		if len(params.RequestID) == 0 || string(params.RequestID) == "null" {
			return fmt.Errorf("Codex resolved no request ID")
		}
		key := string(params.RequestID)
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return s.emit(ctx, conversation.Event{Kind: "permission_resolved", SessionID: thread, RequestID: key, Status: "resolved", Data: message.Params})
	}
	turnID := params.TurnID
	if message.Method == "turn/started" || message.Method == "turn/completed" {
		turnID = params.Turn.ID
	}
	if turnID == "" {
		switch message.Method {
		case "thread/status/changed":
			return s.emit(ctx, conversation.Event{Kind: "state", SessionID: thread, Data: message.Params})
		}
		return nil
	}
	active, err := s.setTurn(turnID)
	if err != nil {
		return err
	}
	if active == nil {
		return nil
	}
	event := conversation.Event{Kind: "state", SessionID: thread, Data: message.Params}
	switch message.Method {
	case "turn/started":
		event.Kind = "turn_started"
	case "turn/completed":
		status := params.Turn.Status
		if status != "completed" && status != "interrupted" && status != "failed" {
			return fmt.Errorf("Codex returned an unknown terminal turn status")
		}
		event.Kind = "turn_completed"
		event.Status = status
		if err = s.emit(ctx, event); err != nil {
			return err
		}
		s.mu.Lock()
		if active.ended {
			s.mu.Unlock()
			return fmt.Errorf("Codex completed a turn twice")
		}
		active.ended = true
		clear(s.pending)
		var terminal error
		if status == "failed" {
			terminal = fmt.Errorf("Codex turn failed; inspect its persisted error event")
		}
		active.done <- completion{status: status, err: terminal}
		close(active.finished)
		s.mu.Unlock()
		return nil
	case "item/agentMessage/delta":
		event.Kind = "text_delta"
		event.Text = params.Delta
		event.MessageID = params.ItemID
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		event.Kind = "reasoning_delta"
		event.Text = params.Delta
		event.MessageID = params.ItemID
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		event.Kind = "tool_output_delta"
		event.Text = params.Delta
		event.ToolCallID = params.ItemID
	case "item/started", "item/completed":
		item := params.Item
		if item.ID == "" || item.Type == "" {
			return fmt.Errorf("Codex item has no identity")
		}
		event.MessageID = item.ID
		event.Status = item.Status
		switch item.Type {
		case "agentMessage":
			event.Kind = "message_snapshot"
			event.Text = item.Text
		case "userMessage":
			event.Kind = "user_snapshot"
		case "reasoning":
			event.Kind = "reasoning_snapshot"
		case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "webSearch", "imageView", "imageGeneration", "sleep":
			event.Kind = "tool_call"
			if message.Method == "item/completed" {
				event.Kind = "tool_update"
			}
			event.ToolCallID = item.ID
			event.Title = item.Type
			if item.Command != "" {
				event.Title = item.Command
			} else if item.Tool != "" {
				event.Title = item.Tool
			}
		default:
			event.Kind = "content"
		}
	}
	return s.emit(ctx, event)
}

func knownNotification(method string) bool {
	switch method {
	case "thread/started", "thread/status/changed", "serverRequest/resolved",
		"turn/started", "turn/completed", "item/started", "item/completed",
		"item/agentMessage/delta", "item/reasoning/textDelta", "item/reasoning/summaryTextDelta",
		"item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		return true
	default:
		return false
	}
}
func (s *Session) request(ctx context.Context, p *providerrpc.Peer, message providerrpc.Message) error {
	unsupported := func() error {
		if err := s.emit(ctx, conversation.Event{Kind: "unsupported_request", SessionID: s.ID(), Status: message.Method}); err != nil {
			return err
		}
		return p.Respond(ctx, message.ID, nil, &providerrpc.RPCError{Code: -32601, Message: "amux does not implement this native client request"})
	}
	switch message.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
	default:
		return unsupported()
	}
	var params struct {
		ThreadID              string `json:"threadId"`
		TurnID                string `json:"turnId"`
		ItemID                string `json:"itemId"`
		Command, Reason, Kind string
		Available             []json.RawMessage `json:"availableDecisions"`
		Permissions           json.RawMessage   `json:"permissions"`
	}
	if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != s.ID() || params.ItemID == "" || params.TurnID == "" {
		return p.Respond(ctx, message.ID, nil, &providerrpc.RPCError{Code: -32602, Message: "approval does not belong to this conversation"})
	}
	active, err := s.setTurn(params.TurnID)
	if err != nil {
		return err
	}
	if active == nil {
		return p.Respond(ctx, message.ID, nil, &providerrpc.RPCError{Code: -32602, Message: "approval has no active turn"})
	}
	if params.Kind == "humanCommand" {
		return unsupported()
	} // Never claim a resolved human principal we do not have.
	request := permission{id: message.ID, choices: map[string]any{}, cancel: map[string]string{"decision": "cancel"}}
	options := []conversation.PermissionOption{}
	add := func(id, name, kind string, response any) {
		request.choices[id] = response
		options = append(options, conversation.PermissionOption{ID: id, Name: name, Kind: kind})
	}
	if message.Method == "item/permissions/requestApproval" {
		var profile map[string]json.RawMessage
		if json.Unmarshal(params.Permissions, &profile) != nil || profile == nil {
			return fmt.Errorf("Codex supplied no permission profile")
		}
		request.cancel = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		add("grantTurn", "Grant requested permissions for this turn", "allow_once", map[string]any{"permissions": params.Permissions, "scope": "turn"})
		add("grantSession", "Grant requested permissions for this session", "allow_always", map[string]any{"permissions": params.Permissions, "scope": "session"})
		add("decline", "Deny permissions", "reject_once", request.cancel)
	} else {
		allowed := params.Available
		legacyChoices := allowed == nil
		if legacyChoices {
			for _, choice := range []string{"accept", "acceptForSession", "decline", "cancel"} {
				raw, _ := json.Marshal(choice)
				allowed = append(allowed, raw)
			}
		}
		if len(allowed) > 128 {
			return fmt.Errorf("Codex approval choice limit exceeded")
		}
		canCancel := legacyChoices
		for _, raw := range allowed {
			var decision string
			if json.Unmarshal(raw, &decision) != nil {
				return unsupported()
			}
			var name, kind string
			switch decision {
			case "accept":
				name = "Allow once"
				kind = "allow_once"
			case "acceptForSession":
				name = "Allow for this session"
				kind = "allow_always"
			case "decline":
				name = "Decline"
				kind = "reject_once"
				canCancel = true
			case "cancel":
				name = "Cancel turn"
				kind = "reject_once"
				canCancel = true
			default:
				return unsupported()
			}
			add(decision, name, kind, map[string]string{"decision": decision})
		}
		if !canCancel || len(options) == 0 {
			return unsupported()
		}
		if !legacyChoices {
			if _, offered := request.choices["cancel"]; !offered {
				request.cancel = request.choices["decline"]
			}
		}
	}
	key := string(message.ID)
	s.mu.Lock()
	if s.closed || s.canceling || active.ended {
		s.mu.Unlock()
		return p.Respond(ctx, message.ID, request.cancel, nil)
	}
	if _, exists := s.pending[key]; exists || len(s.pending) >= 128 {
		s.mu.Unlock()
		return fmt.Errorf("Codex approval identity/queue conflict")
	}
	s.pending[key] = request
	s.mu.Unlock()
	title := params.Command
	if title == "" {
		title = params.Reason
	}
	if title == "" {
		title = message.Method
	}
	return s.emit(ctx, conversation.Event{Kind: "permission", SessionID: params.ThreadID, ToolCallID: params.ItemID, RequestID: key, Title: title, Options: options, Data: message.Params})
}
