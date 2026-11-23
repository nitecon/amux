// Package codex drives native Codex app-server JSONL, not a terminal or a model
// completion API. Turn acknowledgements and execution completion are distinct.
package codex

import (
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

type Config struct {
	Command                    []string
	Directory, ResumeID        string
	Env                        []string
	DeveloperInstructions      string
	MCP                        *providerprocess.MCPConfig
	OpenTimeout, CancelTimeout time.Duration
}
type turn struct {
	id       string
	done     chan completion
	finished chan struct{}
	ended    bool
}
type completion struct {
	status string
	err    error
}
type permission struct {
	id      json.RawMessage
	choices map[string]any
	cancel  any
}
type Session struct {
	ctx            context.Context
	peer           *providerrpc.Peer
	sink           conversation.Sink
	mu             sync.Mutex
	id             string
	active         *turn
	lastTurn       string
	pending        map[string]permission
	canceling      bool
	closed         bool
	cancelTimeout  time.Duration
	eventMu        sync.Mutex
	ready          bool
	bootstrap      []conversation.Event
	bootstrapBytes int
}

func Start(ctx context.Context, config Config, sink conversation.Sink) (*Session, error) {
	if sink == nil {
		return nil, fmt.Errorf("Codex requires an event sink")
	}
	directory, err := providerprocess.ValidateDirectory(config.Directory, "Codex")
	if err != nil {
		return nil, err
	}
	command := config.Command
	if len(command) == 0 {
		command = []string{"codex", "app-server", "--listen", "stdio://"}
	}
	command = appendMCPConfig(command, config.MCP)
	command = appendDeveloperInstructions(command, config.DeveloperInstructions)
	process, err := providerprocess.Start(ctx, providerprocess.Options{Command: command, Directory: directory, Env: config.Env})
	if err != nil {
		return nil, err
	}
	timeout := config.CancelTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	s := &Session{ctx: ctx, sink: sink, id: config.ResumeID, pending: map[string]permission{}, cancelTimeout: timeout}
	s.peer = providerrpc.New(ctx, process, providerrpc.Options{OmitVersion: true}, s.handle)
	fail := func(err error) (*Session, error) { s.Close(); return nil, err }
	timeout = config.OpenTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	openCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err = s.peer.Call(openCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "amux", "title": "amux", "version": "0.0.0"}, "capabilities": map[string]bool{"experimentalApi": false}}); err != nil {
		return fail(err)
	}
	if err = s.peer.Notify(openCtx, "initialized", map[string]any{}); err != nil {
		return fail(err)
	}
	method := "thread/start"
	params := map[string]any{"cwd": directory, "approvalsReviewer": "user"}
	if config.ResumeID != "" {
		method = "thread/resume"
		params["threadId"] = config.ResumeID
		params["excludeTurns"] = true
	}
	raw, err := s.peer.Call(openCtx, method, params)
	if err != nil {
		return fail(err)
	}
	var response struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID == "" {
		return fail(fmt.Errorf("Codex returned no native thread ID"))
	}
	if config.ResumeID != "" && response.Thread.ID != config.ResumeID {
		return fail(fmt.Errorf("Codex resumed a different thread"))
	}
	if response.Thread.Status.Type == "active" || response.Thread.Status.Type == "systemError" {
		return fail(fmt.Errorf("Codex thread requires active-turn recovery before new work"))
	}
	s.mu.Lock()
	if s.id != "" && s.id != response.Thread.ID {
		s.mu.Unlock()
		return fail(fmt.Errorf("Codex thread identity changed during startup"))
	}
	s.id = response.Thread.ID
	s.mu.Unlock()
	if err = s.publishReady(openCtx, conversation.Event{Kind: "session_ready", SessionID: s.ID()}); err != nil {
		return fail(err)
	}
	return s, nil
}

func appendDeveloperInstructions(command []string, instructions string) []string {
	if instructions == "" || hasConfigOverride(command, "developer_instructions") {
		return command
	}
	return append(command, "--config", "developer_instructions="+tomlString(instructions))
}

func hasConfigOverride(args []string, key string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := ""
		switch {
		case arg == "-c" || arg == "--config":
			if i+1 < len(args) {
				i++
				value = args[i]
			}
		case strings.HasPrefix(arg, "--config="):
			value = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			value = strings.TrimPrefix(arg, "-c")
		}
		if overrideKey, _, ok := strings.Cut(value, "="); ok && strings.TrimSpace(overrideKey) == key {
			return true
		}
	}
	return false
}

func appendMCPConfig(command []string, mcp *providerprocess.MCPConfig) []string {
	command = append([]string(nil), command...)
	if mcp == nil {
		return command
	}
	add := func(key, value string) {
		command = append(command, "--config", "mcp_servers.amux."+key+"="+value)
	}
	add("command", tomlString(mcp.Command))
	args := make([]string, len(mcp.Args))
	for i, arg := range mcp.Args {
		args[i] = tomlString(arg)
	}
	add("args", "["+strings.Join(args, ",")+"]")
	envVars := make([]string, len(mcp.EnvVars))
	for i, name := range mcp.EnvVars {
		envVars[i] = tomlString(name)
	}
	add("env_vars", "["+strings.Join(envVars, ",")+"]")
	if !hasConfigOverride(command, "mcp_servers.amux.default_tools_approval_mode") {
		add("default_tools_approval_mode", tomlString("approve"))
	}
	if !hasConfigOverride(command, "tools.update_plan.enabled") {
		command = append(command, "--config", "tools.update_plan.enabled=false")
	}
	return command
}

// JSON string escapes are also valid TOML basic-string escapes.
func tomlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
func (s *Session) ID() string            { s.mu.Lock(); defer s.mu.Unlock(); return s.id }
func (s *Session) Done() <-chan struct{} { return s.peer.Done() }
func (s *Session) Err() error            { return s.peer.Err() }
func (s *Session) Close() error          { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return s.peer.Close() }
func (s *Session) emit(ctx context.Context, event conversation.Event) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if !s.ready {
		raw, _ := json.Marshal(event)
		if len(s.bootstrap) >= 64 || s.bootstrapBytes+len(raw) > 4<<20 {
			return fmt.Errorf("Codex startup event limit exceeded")
		}
		s.bootstrapBytes += len(raw)
		s.bootstrap = append(s.bootstrap, event)
		return nil
	}
	return s.sink(ctx, event)
}
func (s *Session) publishReady(ctx context.Context, event conversation.Event) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := s.sink(ctx, event); err != nil {
		return err
	}
	for _, pending := range s.bootstrap {
		if err := s.sink(ctx, pending); err != nil {
			return err
		}
	}
	s.bootstrap = nil
	s.bootstrapBytes = 0
	s.ready = true
	return nil
}
func (s *Session) setTurn(id string) (*turn, error) {
	if id == "" {
		return nil, fmt.Errorf("Codex event has no turn ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || id == s.lastTurn {
		return nil, nil
	}
	a := s.active
	if a.id == "" {
		a.id = id
	} else if a.id != id {
		return nil, fmt.Errorf("Codex event belongs to another active turn")
	}
	return a, nil
}
func (s *Session) Prompt(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("Codex prompt must be nonempty")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", providerrpc.ErrClosed
	}
	if s.active != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("a Codex turn is already running")
	}
	a := &turn{done: make(chan completion, 1), finished: make(chan struct{})}
	s.active = a
	thread := s.id
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if a.ended {
			s.lastTurn = a.id
		}
		s.active = nil
		s.canceling = false
		clear(s.pending)
		s.mu.Unlock()
	}()
	type startResult struct {
		raw json.RawMessage
		err error
	}
	started := make(chan startResult, 1)
	go func() {
		raw, err := s.peer.Call(s.ctx, "turn/start", map[string]any{"threadId": thread, "input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}})
		started <- startResult{raw, err}
	}()
	var start startResult
	var canceled error
	select {
	case start = <-started:
	case <-ctx.Done():
		canceled = ctx.Err()
		limit, stop := context.WithTimeout(context.WithoutCancel(s.ctx), s.cancelTimeout)
		select {
		case start = <-started:
		case <-limit.Done():
			s.peer.Close()
			stop()
			return "", canceled
		}
		stop()
	}
	if start.err != nil {
		return "", start.err
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(start.raw, &response) != nil || response.Turn.ID == "" {
		s.peer.Close()
		return "", fmt.Errorf("Codex returned no turn ID")
	}
	if _, err := s.setTurn(response.Turn.ID); err != nil {
		s.peer.Close()
		return "", err
	}
	wait := func(waitCtx context.Context) (completion, error) {
		select {
		case result := <-a.done:
			return result, nil
		case <-s.peer.Done():
			select {
			case result := <-a.done:
				return result, nil
			default:
				return completion{}, s.peer.Err()
			}
		case <-waitCtx.Done():
			return completion{}, waitCtx.Err()
		}
	}
	if canceled == nil {
		result, err := wait(ctx)
		if err == nil {
			return result.status, result.err
		}
		if ctx.Err() == nil {
			return "", err
		}
		canceled = ctx.Err()
	}
	limit, stop := context.WithTimeout(context.WithoutCancel(s.ctx), s.cancelTimeout)
	defer stop()
	if err := s.interrupt(limit, a); err != nil {
		s.peer.Close()
		return "", canceled
	}
	result, err := wait(limit)
	if err != nil {
		s.peer.Close()
		return "", canceled
	}
	return result.status, canceled
}
func (s *Session) interrupt(ctx context.Context, a *turn) error {
	s.mu.Lock()
	if a.ended {
		s.mu.Unlock()
		return nil
	}
	s.canceling = true
	requests := s.pending
	s.pending = map[string]permission{}
	id := a.id
	thread := s.id
	s.mu.Unlock()
	for _, request := range requests {
		if err := s.peer.Respond(ctx, request.id, request.cancel, nil); err != nil {
			if s.turnFinished(a) {
				return nil
			}
			return err
		}
		if s.turnFinished(a) {
			return nil
		}
	}
	if id == "" {
		return fmt.Errorf("Codex turn has not acquired a native ID")
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	callResult := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(callCtx, "turn/interrupt", map[string]string{"threadId": thread, "turnId": id})
		callResult <- err
	}()
	select {
	case <-a.finished:
		return nil
	case err := <-callResult:
		if err != nil && s.turnFinished(a) {
			return nil
		}
		return err
	case <-ctx.Done():
		if s.turnFinished(a) {
			return nil
		}
		return ctx.Err()
	}
}

func (s *Session) turnFinished(a *turn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return a.ended
}
func (s *Session) ResolvePermission(ctx context.Context, requestID, optionID string) error {
	s.mu.Lock()
	request, ok := s.pending[requestID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("Codex approval is no longer pending")
	}
	response := request.cancel
	if optionID != "" {
		var offered bool
		response, offered = request.choices[optionID]
		if !offered {
			s.mu.Unlock()
			return fmt.Errorf("Codex approval option was not offered")
		}
	}
	delete(s.pending, requestID)
	s.mu.Unlock()
	if err := s.peer.Respond(ctx, request.id, response, nil); err != nil {
		s.peer.Close()
		return err
	}
	return nil
}
