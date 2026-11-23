// Package claude drives Claude Code's native bidirectional stream-json CLI.
// Its stream frames and control envelopes are not JSON-RPC.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providerprocess"
)

type Config struct {
	Directory, ResumeID string
	Command             []string
	Env                 []string
	MCP                 *providerprocess.MCPConfig
	OpenTimeout         time.Duration
	CancelTimeout       time.Duration
}

const sessionSettings = `{"attribution":{"commit":"","pr":""},"includeCoAuthoredBy":false,"permissions":{"allow":["mcp__amux__*"],"deny":["TaskCreate","TaskGet","TaskList","TaskUpdate","TodoWrite"]}}`

type controlRequest struct {
	response   chan controlResult
	permission *permission
}
type controlResult struct {
	response json.RawMessage
	err      error
}
type permission struct {
	input json.RawMessage
}
type turn struct {
	done  chan turnResult
	ended bool
}
type turnResult struct {
	status string
	err    error
}

type Session struct {
	ctx           context.Context
	cancel        context.CancelFunc
	process       *providerprocess.Process
	sink          conversation.Sink
	mu            sync.Mutex
	eventMu       sync.Mutex
	writeGate     chan struct{}
	id            string
	active        *turn
	pending       map[string]controlRequest
	closed        bool
	ready         bool
	startupEvents []conversation.Event
	startupBytes  int
	readerErr     error
	readerDone    chan struct{}
	messageID     string
	toolByBlock   map[int]string
	openTimeout   time.Duration
	cancelTimeout time.Duration
	requestSeq    uint64
}

const (
	maxFrameBytes       = 4 << 20
	maxPendingControls  = 128
	maxStartupEvents    = 64
	maxStartupEventSize = 4 << 20
)

func Start(ctx context.Context, config Config, sink conversation.Sink) (*Session, error) {
	if sink == nil {
		return nil, fmt.Errorf("Claude session requires an event sink")
	}
	directory, err := providerprocess.ValidateDirectory(config.Directory, "Claude")
	if err != nil {
		return nil, err
	}
	command := append([]string(nil), config.Command...)
	if len(command) == 0 {
		command = []string{"claude"}
	}
	command = appendMCPConfig(command, config.MCP)
	command = append(command, "--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompts", "host")
	if config.ResumeID != "" {
		command = append(command, "--resume", config.ResumeID)
	} else {
		config.ResumeID = uuid.NewString()
		command = append(command, "--session-id", config.ResumeID)
	}
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	process, err := providerprocess.Start(sessionCtx, providerprocess.Options{Command: command, Directory: directory, Env: config.Env})
	if err != nil {
		sessionCancel()
		return nil, err
	}
	writeGate := make(chan struct{}, 1)
	writeGate <- struct{}{}
	s := &Session{ctx: sessionCtx, cancel: sessionCancel, process: process, sink: sink, id: config.ResumeID, pending: map[string]controlRequest{}, readerDone: make(chan struct{}), writeGate: writeGate, toolByBlock: map[int]string{}, openTimeout: config.OpenTimeout, cancelTimeout: config.CancelTimeout}
	if s.openTimeout <= 0 {
		s.openTimeout = 15 * time.Second
	}
	if s.cancelTimeout <= 0 {
		s.cancelTimeout = 5 * time.Second
	}
	go s.readLoop()
	fail := func(err error) (*Session, error) { s.Close(); return nil, err }
	openCtx, cancel := context.WithTimeout(sessionCtx, s.openTimeout)
	defer cancel()
	if _, err = s.requestControl(openCtx, map[string]any{"subtype": "initialize"}); err != nil {
		return fail(fmt.Errorf("initialize Claude stream protocol: %w", err))
	}
	select {
	case <-s.readerDone:
		return fail(fmt.Errorf("Claude stream failed during startup: %w", readErrOrClosed(s.Err())))
	default:
	}
	s.eventMu.Lock()
	id := s.ID()
	if err := sink(openCtx, conversation.Event{Kind: "session_ready", SessionID: id}); err != nil {
		s.eventMu.Unlock()
		return fail(err)
	}
	for _, event := range s.startupEvents {
		if err := sink(openCtx, event); err != nil {
			s.eventMu.Unlock()
			return fail(err)
		}
	}
	s.startupEvents = nil
	s.startupBytes = 0
	s.ready = true
	s.eventMu.Unlock()
	return s, nil
}

func appendMCPConfig(command []string, mcp *providerprocess.MCPConfig) []string {
	command = append([]string(nil), command...)
	if mcp == nil {
		return command
	}
	command = append(command, "--settings", sessionSettings)
	server := map[string]any{
		"command": mcp.Command,
		"args":    mcp.Args,
	}
	if len(mcp.Env) > 0 {
		server["env"] = mcp.Env
	}
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"amux": server}})
	return append(command, "--mcp-config", string(config))
}

func (s *Session) ID() string            { s.mu.Lock(); defer s.mu.Unlock(); return s.id }
func (s *Session) Done() <-chan struct{} { return s.readerDone }
func (s *Session) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.readerErr }
func (s *Session) Close() error {
	err := s.abort()
	<-s.readerDone
	return err
}

// abort retires the child process without waiting for the protocol reader.
// It is also used by the write watcher, which can run while that reader is
// blocked writing a response to the child.
func (s *Session) abort() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	return s.process.Close()
}

func (s *Session) Prompt(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("Claude prompt must be nonempty")
	}
	select {
	case <-s.readerDone:
		return "", readErrOrClosed(s.Err())
	default:
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("Claude session is closed")
	}
	if s.active != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("a Claude turn is already running")
	}
	a := &turn{done: make(chan turnResult, 1)}
	s.active = a
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.active == a {
			s.active = nil
		}
		for key, request := range s.pending {
			if request.permission != nil {
				delete(s.pending, key)
			}
		}
		s.mu.Unlock()
	}()
	if err := s.writeContext(ctx, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}); err != nil {
		return "", err
	}
	select {
	case result := <-a.done:
		return result.status, result.err
	case <-ctx.Done():
		select {
		case result := <-a.done:
			return result.status, result.err
		default:
		}
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), s.cancelTimeout)
		defer cancel()
		_, cancelErr := s.requestControl(cancelCtx, map[string]any{"subtype": "interrupt"})
		if cancelErr != nil {
			s.Close()
			return "", ctx.Err()
		}
		select {
		case <-a.done:
		case <-cancelCtx.Done():
			s.Close()
		}
		return "", ctx.Err()
	case <-s.readerDone:
		select {
		case result := <-a.done:
			return result.status, result.err
		default:
		}
		return "", readErrOrClosed(s.Err())
	}
}

func (s *Session) ResolvePermission(ctx context.Context, requestID, optionID string) error {
	s.mu.Lock()
	request, ok := s.pending[requestID]
	if !ok || request.permission == nil {
		s.mu.Unlock()
		return fmt.Errorf("Claude permission request is no longer pending")
	}
	if optionID != "allow-once" && optionID != "deny" {
		s.mu.Unlock()
		return fmt.Errorf("Claude permission option was not offered")
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	delete(s.pending, requestID)
	s.mu.Unlock()
	response := map[string]any{"behavior": "deny", "message": "Permission denied by user"}
	if optionID == "allow-once" {
		response = map[string]any{"behavior": "allow", "updatedInput": json.RawMessage(request.permission.input)}
	}
	if err := s.respondControl(ctx, requestID, "success", response, ""); err != nil {
		_ = s.abort()
		return err
	}
	return nil
}

func (s *Session) requestControl(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	s.requestSeq++
	id := fmt.Sprintf("amux-%d", s.requestSeq)
	result := make(chan controlResult, 1)
	if len(s.pending) >= maxPendingControls {
		s.mu.Unlock()
		return nil, fmt.Errorf("Claude pending control request limit reached")
	}
	s.pending[id] = controlRequest{response: result}
	s.mu.Unlock()
	if err := s.writeContext(ctx, map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case response := <-result:
		return response.response, response.err
	case <-ctx.Done():
		select {
		case response := <-result:
			return response.response, response.err
		default:
		}
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-s.readerDone:
		select {
		case response := <-result:
			return response.response, response.err
		default:
		}
		return nil, readErrOrClosed(s.Err())
	}
}

func (s *Session) writeContext(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	select {
	case <-s.writeGate:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { s.writeGate <- struct{}{} }()
	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-finished:
		case <-ctx.Done():
			select {
			case <-finished:
				return
			default:
				_ = s.abort()
			}
		}
	}()
	for len(data) > 0 {
		n, writeErr := s.process.Write(data)
		if writeErr != nil {
			close(finished)
			<-watcherDone
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return writeErr
		}
		if n == 0 {
			close(finished)
			<-watcherDone
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	close(finished)
	<-watcherDone
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (s *Session) readLoop() {
	defer close(s.readerDone)
	reader := bufio.NewScanner(s.process)
	reader.Buffer(make([]byte, 64*1024), maxFrameBytes)
	var readErr error
	for reader.Scan() {
		line := bytes.TrimSpace(reader.Bytes())
		if len(line) == 0 {
			continue
		}
		if handleErr := s.handleLine(line); handleErr != nil {
			readErr = handleErr
			break
		}
	}
	if readErr == nil && reader.Err() != nil {
		readErr = fmt.Errorf("read Claude stream frame (maximum %d bytes): %w", maxFrameBytes, reader.Err())
	}
	if processErr := s.process.Close(); readErr == nil && processErr != nil {
		readErr = processErr
	}
	s.mu.Lock()
	s.readerErr = readErr
	for id, request := range s.pending {
		delete(s.pending, id)
		if request.response != nil {
			request.response <- controlResult{err: readErrOrClosed(readErr)}
		}
	}
	a := s.active
	if a != nil && !a.ended {
		a.ended = true
		a.done <- turnResult{err: readErrOrClosed(readErr)}
	}
	s.mu.Unlock()
}

func readErrOrClosed(err error) error {
	if err != nil {
		return err
	}
	return io.EOF
}

func (s *Session) handleLine(line []byte) error {
	var message struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		SessionID string          `json:"session_id"`
		UUID      string          `json:"uuid"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
		Response  json.RawMessage `json:"response"`
		Event     json.RawMessage `json:"event"`
		Message   json.RawMessage `json:"message"`
		Result    string          `json:"result"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(line, &message); err != nil {
		return fmt.Errorf("invalid Claude stream-json frame: %w", err)
	}
	if message.Type == "" {
		return fmt.Errorf("Claude stream-json frame has no type")
	}
	switch message.Type {
	case "control_response":
		var response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		}
		if json.Unmarshal(message.Response, &response) != nil || response.RequestID == "" {
			return fmt.Errorf("invalid Claude control response")
		}
		s.mu.Lock()
		request, ok := s.pending[response.RequestID]
		if ok {
			delete(s.pending, response.RequestID)
		}
		s.mu.Unlock()
		if !ok {
			return nil
		}
		if request.response != nil {
			if response.Subtype == "error" {
				request.response <- controlResult{err: fmt.Errorf("Claude control request: %s", response.Error)}
			} else if response.Subtype != "success" {
				request.response <- controlResult{err: fmt.Errorf("unknown Claude control response %q", response.Subtype)}
			} else {
				request.response <- controlResult{response: response.Response}
			}
		}
		return nil
	case "control_request":
		return s.handleControlRequest(message.RequestID, message.Request)
	case "system":
		if message.Subtype == "init" {
			if message.SessionID == "" {
				return fmt.Errorf("Claude initialization has no session ID")
			}
			s.mu.Lock()
			if s.id != "" && s.id != message.SessionID {
				s.mu.Unlock()
				return fmt.Errorf("Claude session identity changed during startup")
			}
			s.id = message.SessionID
			s.mu.Unlock()
		}
		return s.emit(conversation.Event{Kind: "state", SessionID: s.ID(), Status: message.Subtype, Data: append(json.RawMessage(nil), line...)})
	case "stream_event":
		return s.handleStreamEvent(message.SessionID, message.UUID, message.Event)
	case "assistant":
		return s.handleAssistant(message.SessionID, message.Message)
	case "user":
		return s.handleUser(message.SessionID, message.Message)
	case "result":
		if message.SessionID == "" || message.SessionID != s.ID() {
			return fmt.Errorf("Claude result belongs to a different session")
		}
		status := message.Subtype
		if status == "" {
			status = "completed"
		}
		if err := s.emit(conversation.Event{Kind: "turn_completed", SessionID: message.SessionID, Status: status, Text: message.Result, Data: append(json.RawMessage(nil), line...)}); err != nil {
			return err
		}
		s.mu.Lock()
		a := s.active
		if a != nil && !a.ended {
			a.ended = true
			var turnErr error
			if message.IsError || strings.HasPrefix(status, "error") {
				turnErr = fmt.Errorf("Claude turn failed")
			}
			a.done <- turnResult{status: status, err: turnErr}
		}
		s.mu.Unlock()
		return nil
	default:
		return s.emit(conversation.Event{Kind: "state", SessionID: s.ID(), Status: message.Type, Data: append(json.RawMessage(nil), line...)})
	}
}

func (s *Session) handleControlRequest(requestID string, raw json.RawMessage) error {
	if requestID == "" {
		return fmt.Errorf("Claude control request has no request ID")
	}
	var request struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		ToolUseID string          `json:"tool_use_id"`
		Title     string          `json:"title"`
		Input     json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Subtype == "" {
		return s.respondControl(s.ctx, requestID, "error", nil, "malformed Claude control request")
	}
	switch request.Subtype {
	case "initialize":
		return s.respondControl(s.ctx, requestID, "success", map[string]any{"commands": []any{}, "models": []any{}, "agents": []any{}, "account": nil}, "")
	case "can_use_tool":
		if request.ToolName == "" || request.ToolUseID == "" || len(request.Input) == 0 || !json.Valid(request.Input) {
			return s.respondControl(s.ctx, requestID, "error", nil, "malformed Claude permission request")
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return s.respondControl(s.ctx, requestID, "success", map[string]any{"behavior": "deny", "message": "Session is closing"}, "")
		}
		if _, exists := s.pending[requestID]; exists {
			s.mu.Unlock()
			return fmt.Errorf("duplicate Claude control request ID")
		}
		if len(s.pending) >= maxPendingControls {
			s.mu.Unlock()
			return s.respondControl(s.ctx, requestID, "error", nil, "Claude pending control request limit reached")
		}
		p := &permission{input: append(json.RawMessage(nil), request.Input...)}
		s.pending[requestID] = controlRequest{permission: p}
		s.mu.Unlock()
		title := request.Title
		if title == "" {
			title = request.ToolName
		}
		return s.emit(conversation.Event{Kind: "permission", SessionID: s.ID(), RequestID: requestID, ToolCallID: request.ToolUseID, Title: title, Data: append(json.RawMessage(nil), raw...), Options: []conversation.PermissionOption{{ID: "allow-once", Name: "Allow once", Kind: "allow_once"}, {ID: "deny", Name: "Deny", Kind: "reject_once"}}})
	default:
		return s.respondControl(s.ctx, requestID, "error", nil, "unsupported Claude control request subtype: "+request.Subtype)
	}
}

func (s *Session) respondControl(ctx context.Context, requestID, subtype string, response any, message string) error {
	envelope := map[string]any{"type": "control_response", "response": map[string]any{"subtype": subtype, "request_id": requestID}}
	fields := envelope["response"].(map[string]any)
	if subtype == "error" {
		fields["error"] = message
	} else {
		fields["response"] = response
	}
	return s.writeContext(ctx, envelope)
}
func (s *Session) emit(event conversation.Event) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if !s.ready {
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if len(s.startupEvents) >= maxStartupEvents || s.startupBytes+len(encoded) > maxStartupEventSize {
			return fmt.Errorf("Claude startup event buffer limit exceeded")
		}
		s.startupBytes += len(encoded)
		s.startupEvents = append(s.startupEvents, event)
		return nil
	}
	return s.sink(s.ctx, event)
}

func (s *Session) handleStreamEvent(sessionID, uuid string, raw json.RawMessage) error {
	var stream struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &stream) != nil || stream.Type == "" {
		return fmt.Errorf("invalid Claude stream event")
	}
	if sessionID == "" {
		sessionID = s.ID()
	}
	if sessionID != s.ID() {
		return nil
	}
	if stream.Type == "message_start" && stream.Message.ID != "" {
		s.messageID = stream.Message.ID
		s.toolByBlock = map[int]string{}
	}
	data, err := json.Marshal(struct {
		UUID  string          `json:"uuid"`
		Event json.RawMessage `json:"event"`
	}{UUID: uuid, Event: raw})
	if err != nil {
		return err
	}
	event := conversation.Event{SessionID: sessionID, MessageID: s.messageID, Data: data}
	switch stream.Type {
	case "content_block_start":
		if stream.ContentBlock.Type == "tool_use" {
			event.Kind = "tool_call"
			event.ToolCallID = stream.ContentBlock.ID
			event.Title = stream.ContentBlock.Name
			s.toolByBlock[stream.Index] = stream.ContentBlock.ID
		}
	case "content_block_delta":
		if stream.Delta.Type == "text_delta" {
			event.Kind = "text_delta"
			event.Text = stream.Delta.Text
		} else if stream.Delta.Type == "input_json_delta" {
			event.Kind = "tool_update"
			event.ToolCallID = s.toolByBlock[stream.Index]
			if event.ToolCallID == "" {
				return fmt.Errorf("Claude tool input delta has no matching tool block")
			}
		}
	case "content_block_stop":
		return nil
	case "message_start", "message_delta", "message_stop":
		event.Kind = "state"
		event.Status = stream.Type
		if stream.Type == "message_stop" {
			s.messageID = ""
			s.toolByBlock = map[int]string{}
		}
	default:
		event.Kind = "state"
		event.Status = stream.Type
	}
	if event.Kind == "" {
		return nil
	}
	return s.emit(event)
}

func (s *Session) handleAssistant(sessionID string, raw json.RawMessage) error {
	if sessionID == "" {
		sessionID = s.ID()
	}
	if sessionID != s.ID() {
		return nil
	}
	var assistant struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &assistant) != nil {
		return fmt.Errorf("invalid Claude assistant message")
	}
	var events []conversation.Event
	var text strings.Builder
	hasText := false
	for _, block := range assistant.Content {
		event := conversation.Event{SessionID: sessionID, MessageID: assistant.ID, Data: append(json.RawMessage(nil), raw...)}
		switch block.Type {
		case "text":
			if !hasText {
				event.Kind = "message_snapshot"
				events = append(events, event)
				hasText = true
			}
			text.WriteString(block.Text)
		case "tool_use":
			event.Kind = "tool_call"
			event.ToolCallID = block.ID
			event.Title = block.Name
			events = append(events, event)
		default:
			event.Kind = "content"
			events = append(events, event)
		}
	}
	if hasText {
		for i := range events {
			if events[i].Kind == "message_snapshot" {
				events[i].Text = text.String()
				break
			}
		}
	}
	for _, event := range events {
		if err := s.emit(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) handleUser(sessionID string, raw json.RawMessage) error {
	if sessionID == "" {
		sessionID = s.ID()
	}
	if sessionID != s.ID() {
		return nil
	}
	var user struct {
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   any    `json:"content"`
			IsError   bool   `json:"is_error"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &user) != nil {
		return fmt.Errorf("invalid Claude user message")
	}
	for _, block := range user.Content {
		if block.Type == "tool_result" {
			status := "completed"
			if block.IsError {
				status = "failed"
			}
			data, _ := json.Marshal(block.Content)
			if err := s.emit(conversation.Event{Kind: "tool_update", SessionID: sessionID, ToolCallID: block.ToolUseID, Status: status, Data: data}); err != nil {
				return err
			}
		}
	}
	return nil
}
