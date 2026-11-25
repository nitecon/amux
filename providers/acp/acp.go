// Package acp integrates native Agent Client Protocol v1 agents, including
// Antigravity's `agy acp`. It advertises no editor filesystem or terminal access.
package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providerprocess"
	"github.com/nitecon/amux/providerrpc"
)

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type MCPServer struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []EnvVar `json:"env"`
}
type Config struct {
	Command         []string
	Directory       string
	ResumeID        string
	Env             []string
	MCPServers      []MCPServer
	AutoApproveTool func(name string) bool
	OpenTimeout     time.Duration
	CancelTimeout   time.Duration
}
type permission struct {
	id      json.RawMessage
	options map[string]bool
}
type Session struct {
	ctx             context.Context
	peer            *providerrpc.Peer
	sink            conversation.Sink
	mu              sync.Mutex
	id              string
	pending         map[string]permission
	autoApproveTool func(name string) bool
	busy            bool
	canceling       bool
	closed          bool
	cancelTimeout   time.Duration
}

// Start owns a native process and session for the lifetime of ctx. A supplied
// resume ID is exact: unsupported or failed resume never starts fresh instead.
func Start(ctx context.Context, config Config, sink conversation.Sink) (*Session, error) {
	if sink == nil {
		return nil, fmt.Errorf("ACP session requires an event sink")
	}
	directory, err := providerprocess.ValidateDirectory(config.Directory, "ACP")
	if err != nil {
		return nil, err
	}
	command := config.Command
	if len(command) == 0 {
		command = []string{"agy", "acp", "--stdio"}
	}
	process, err := providerprocess.Start(ctx, providerprocess.Options{Command: command, Directory: directory, Env: config.Env})
	if err != nil {
		return nil, err
	}
	cancelTimeout := config.CancelTimeout
	if cancelTimeout <= 0 {
		cancelTimeout = 5 * time.Second
	}
	session := &Session{ctx: ctx, sink: sink, id: config.ResumeID, pending: map[string]permission{}, cancelTimeout: cancelTimeout, autoApproveTool: config.AutoApproveTool}
	session.peer = providerrpc.New(ctx, process, providerrpc.Options{}, session.handle)
	fail := func(err error) (*Session, error) { session.Close(); return nil, err }
	timeout := config.OpenTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	openCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := session.peer.Call(openCtx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}, "clientInfo": map[string]string{"name": "amux", "version": "0.0.0"}})
	if err != nil {
		return fail(err)
	}
	var initialized struct {
		Version      uint32 `json:"protocolVersion"`
		Capabilities struct {
			Load     bool `json:"loadSession"`
			Sessions struct {
				Resume json.RawMessage `json:"resume"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(raw, &initialized) != nil || initialized.Version != 1 {
		return fail(fmt.Errorf("agent did not negotiate ACP protocol version 1"))
	}
	servers := append([]MCPServer(nil), config.MCPServers...)
	if servers == nil {
		servers = []MCPServer{}
	}
	for i := range servers {
		if servers[i].Args == nil {
			servers[i].Args = []string{}
		}
		if servers[i].Env == nil {
			servers[i].Env = []EnvVar{}
		}
	}
	params := map[string]any{"cwd": directory, "mcpServers": servers}
	method := "session/new"
	if config.ResumeID != "" {
		params["sessionId"] = config.ResumeID
		resume := bytes.TrimSpace(initialized.Capabilities.Sessions.Resume)
		if len(resume) > 0 && resume[0] == '{' {
			method = "session/resume"
		} else if initialized.Capabilities.Load {
			method = "session/load"
		} else {
			return fail(fmt.Errorf("agent cannot resume the exact saved ACP session"))
		}
	}
	result, err := session.peer.Call(openCtx, method, params)
	if err != nil {
		return fail(err)
	}
	if config.ResumeID == "" {
		var created struct {
			ID string `json:"sessionId"`
		}
		if json.Unmarshal(result, &created) != nil || strings.TrimSpace(created.ID) == "" {
			return fail(fmt.Errorf("agent returned no ACP session ID"))
		}
		session.mu.Lock()
		session.id = created.ID
		session.mu.Unlock()
	}
	if err := session.emit(openCtx, conversation.Event{Kind: "session_ready", SessionID: session.ID(), Data: raw}); err != nil {
		return fail(err)
	}
	return session, nil
}
func (s *Session) ID() string            { s.mu.Lock(); defer s.mu.Unlock(); return s.id }
func (s *Session) Done() <-chan struct{} { return s.peer.Done() }
func (s *Session) Err() error            { return s.peer.Err() }
func (s *Session) Close() error          { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return s.peer.Close() }
func (s *Session) emit(ctx context.Context, event conversation.Event) error {
	return s.sink(ctx, event)
}

func (s *Session) Prompt(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("ACP prompt must be non-empty")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", providerrpc.ErrClosed
	}
	if s.busy {
		s.mu.Unlock()
		return "", fmt.Errorf("an ACP turn is already running")
	}
	s.busy = true
	id := s.id
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.busy = false; s.canceling = false; s.mu.Unlock() }()
	type response struct {
		raw json.RawMessage
		err error
	}
	done := make(chan response, 1)
	go func() {
		raw, err := s.peer.Call(s.ctx, "session/prompt", map[string]any{"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": text}}})
		done <- response{raw, err}
	}()
	var result response
	var canceled error
	select {
	case result = <-done:
	case <-ctx.Done():
		canceled = ctx.Err()
		cancelCtx, stop := context.WithTimeout(context.WithoutCancel(s.ctx), s.cancelTimeout)
		err := s.Cancel(cancelCtx)
		if err != nil {
			s.peer.Close()
			result = <-done
			stop()
			return "", canceled
		}
		select {
		case result = <-done:
		case <-cancelCtx.Done():
			s.peer.Close()
			result = <-done
		}
		stop()
	}
	if result.err != nil {
		if canceled != nil {
			return "", canceled
		}
		return "", result.err
	}
	var completed struct {
		Reason string `json:"stopReason"`
	}
	if json.Unmarshal(result.raw, &completed) != nil || completed.Reason == "" {
		return "", fmt.Errorf("agent returned no ACP stop reason")
	}
	if err := s.emit(s.ctx, conversation.Event{Kind: "turn_completed", SessionID: id, Status: completed.Reason, Data: result.raw}); err != nil {
		return "", err
	}
	if canceled != nil {
		return completed.Reason, canceled
	}
	return completed.Reason, nil
}

func (s *Session) Cancel(ctx context.Context) error {
	s.mu.Lock()
	if !s.busy {
		s.mu.Unlock()
		return nil
	}
	id := s.id
	s.canceling = true
	pending := s.pending
	s.pending = map[string]permission{}
	s.mu.Unlock()
	if err := s.peer.Notify(ctx, "session/cancel", map[string]string{"sessionId": id}); err != nil {
		return err
	}
	for _, request := range pending {
		if err := s.peer.Respond(ctx, request.id, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil); err != nil {
			return err
		}
	}
	return nil
}
func (s *Session) ResolvePermission(ctx context.Context, requestID, optionID string) error {
	s.mu.Lock()
	request, exists := s.pending[requestID]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("ACP permission request is no longer pending")
	}
	if optionID != "" && !request.options[optionID] {
		s.mu.Unlock()
		return fmt.Errorf("ACP permission option was not offered")
	}
	delete(s.pending, requestID)
	s.mu.Unlock()
	outcome := map[string]string{"outcome": "cancelled"}
	if optionID != "" {
		outcome = map[string]string{"outcome": "selected", "optionId": optionID}
	}
	if err := s.peer.Respond(ctx, request.id, map[string]any{"outcome": outcome}, nil); err != nil {
		s.peer.Close()
		return err
	}
	return nil
}

func (s *Session) handle(ctx context.Context, p *providerrpc.Peer, message providerrpc.Message) error {
	switch message.Method {
	case "session/update":
		var params struct {
			SessionID string          `json:"sessionId"`
			Update    json.RawMessage `json:"update"`
		}
		if json.Unmarshal(message.Params, &params) != nil || params.SessionID == "" || params.SessionID != s.ID() {
			return fmt.Errorf("ACP update belongs to a different session")
		}
		var update struct {
			Type      string          `json:"sessionUpdate"`
			MessageID string          `json:"messageId"`
			ToolID    string          `json:"toolCallId"`
			Title     string          `json:"title"`
			Status    string          `json:"status"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(params.Update, &update) != nil || update.Type == "" {
			return fmt.Errorf("invalid ACP session update")
		}
		event := conversation.Event{Kind: "state", SessionID: params.SessionID, MessageID: update.MessageID, ToolCallID: update.ToolID, Title: update.Title, Status: update.Status, Data: params.Update}
		switch update.Type {
		case "agent_message_chunk", "user_message_chunk", "agent_thought_chunk":
			event.Kind = "text_delta"
			if update.Type == "user_message_chunk" {
				event.Kind = "user_delta"
			}
			if update.Type == "agent_thought_chunk" {
				event.Kind = "reasoning_delta"
			}
			var content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(update.Content, &content) != nil {
				return fmt.Errorf("invalid ACP content")
			}
			if content.Type == "text" {
				event.Text = content.Text
			} else {
				event.Kind = "content"
			}
		case "tool_call":
			event.Kind = "tool_call"
		case "tool_call_update":
			event.Kind = "tool_update"
		}
		return s.emit(ctx, event)
	case "session/request_permission":
		if message.ID == nil {
			return fmt.Errorf("ACP permission request has no RPC ID")
		}
		var params struct {
			SessionID string `json:"sessionId"`
			ToolCall  struct {
				ID    string `json:"toolCallId"`
				Name  string `json:"name"`
				Title string `json:"title"`
			} `json:"toolCall"`
			Options []struct {
				ID   string `json:"optionId"`
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"options"`
		}
		if json.Unmarshal(message.Params, &params) != nil || params.SessionID != s.ID() || params.SessionID == "" || params.ToolCall.ID == "" || len(params.Options) == 0 {
			return fmt.Errorf("invalid ACP permission request")
		}
		options := map[string]bool{}
		choices := []conversation.PermissionOption{}
		for _, option := range params.Options {
			if option.ID == "" || options[option.ID] {
				return fmt.Errorf("invalid ACP permission options")
			}
			options[option.ID] = true
			choices = append(choices, conversation.PermissionOption{ID: option.ID, Name: option.Name, Kind: option.Kind})
		}
		key := string(message.ID)
		s.mu.Lock()
		if s.canceling || s.closed {
			s.mu.Unlock()
			return p.Respond(ctx, message.ID, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil)
		}
		if len(s.pending) >= 128 {
			s.mu.Unlock()
			return fmt.Errorf("ACP permission request limit reached")
		}
		if _, exists := s.pending[key]; exists {
			s.mu.Unlock()
			return fmt.Errorf("duplicate ACP permission request ID")
		}
		if s.autoApproveTool != nil && s.autoApproveTool(params.ToolCall.Name) {
			for _, option := range choices {
				if option.Kind == "allow_once" {
					s.mu.Unlock()
					return p.Respond(ctx, message.ID, map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": option.ID}}, nil)
				}
			}
		}
		s.pending[key] = permission{id: message.ID, options: options}
		s.mu.Unlock()
		return s.emit(ctx, conversation.Event{Kind: "permission", SessionID: params.SessionID, ToolCallID: params.ToolCall.ID, RequestID: key, Title: params.ToolCall.Title, Data: message.Params, Options: choices})
	default:
		if message.ID != nil {
			return p.Respond(ctx, message.ID, nil, &providerrpc.RPCError{Code: -32601, Message: "client method not supported"})
		}
		return nil
	}
}
