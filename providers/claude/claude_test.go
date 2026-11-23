package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestMain(m *testing.M) {
	if os.Getenv("AMUX_CLAUDE_FIXTURE") == "1" {
		os.Exit(runFixture())
	}
	os.Exit(m.Run())
}

func runFixture() int {
	sessionID := "native-new-session"
	args := strings.Join(os.Args, " ")
	if strings.Contains(args, "--resume saved-session") {
		if strings.Contains(args, "--session-id") {
			return 19
		}
		sessionID = "saved-session"
	} else if at := strings.Index(args, "--session-id "); at >= 0 {
		rest := strings.Fields(args[at+len("--session-id "):])
		if len(rest) == 0 {
			return 20
		}
		sessionID = rest[0]
	}
	if os.Getenv("AMUX_CLAUDE_MISMATCH") == "1" {
		sessionID = "fresh-session"
	}
	input := bufio.NewScanner(os.Stdin)
	output := json.NewEncoder(os.Stdout)
	send := func(v any) bool { return output.Encode(v) == nil }
	for input.Scan() {
		var frame struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Message   struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(input.Bytes(), &frame) != nil {
			return 21
		}
		switch frame.Type {
		case "control_request":
			var request struct {
				Subtype string `json:"subtype"`
			}
			if json.Unmarshal(frame.Request, &request) != nil {
				return 22
			}
			response := map[string]any{}
			if request.Subtype == "initialize" {
				response = map[string]any{"commands": []any{}, "models": []any{}, "agents": []any{}, "account": nil}
			} else if request.Subtype == "interrupt" {
				if !send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": frame.RequestID, "response": response}}) {
					return 23
				}
				if !send(map[string]any{"type": "result", "subtype": "interrupted", "session_id": sessionID, "is_error": false, "result": "stopped"}) {
					return 24
				}
				return 0
			} else {
				return 25
			}
			if !send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": frame.RequestID, "response": response}}) {
				return 26
			}
			if os.Getenv("AMUX_CLAUDE_OVERSIZE") == "1" {
				_ = send(map[string]any{"type": "system", "subtype": "oversize", "payload": strings.Repeat("x", maxFrameBytes+1)})
				return 41
			}
			if os.Getenv("AMUX_CLAUDE_STALLED_CONTROL") == "1" {
				for i := 0; i < 10000; i++ {
					if !send(map[string]any{"type": "control_request", "request_id": fmt.Sprintf("stall-%d", i), "request": map[string]any{"subtype": "initialize"}}) {
						return 42
					}
				}
				return 0
			}
			if os.Getenv("AMUX_CLAUDE_UNSUPPORTED_CONTROL") == "1" {
				if !send(map[string]any{"type": "control_request", "request_id": "unsupported-1", "request": map[string]any{"subtype": "hook_callback", "callback_id": "unregistered"}}) {
					return 43
				}
				var response struct {
					Type     string `json:"type"`
					Response struct {
						Subtype   string `json:"subtype"`
						RequestID string `json:"request_id"`
					} `json:"response"`
				}
				if !input.Scan() || json.Unmarshal(input.Bytes(), &response) != nil || response.Type != "control_response" || response.Response.Subtype != "error" || response.Response.RequestID != "unsupported-1" {
					return 44
				}
				if !send(map[string]any{"type": "system", "subtype": "unsupported_control_answered", "session_id": sessionID}) {
					return 45
				}
				continue
			}
		case "user":
			if !send(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID}) {
				return 27
			}
			if frame.Message.Content == "cancel" {
				continue
			}
			if frame.Message.Content == "permission" {
				if !send(map[string]any{"type": "control_request", "request_id": "native-control-7", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "tool_use_id": "tool-native-2", "input": map[string]any{"file_path": "fixture.txt"}}}) {
					return 28
				}
				var response struct {
					Type     string `json:"type"`
					Response struct {
						RequestID string `json:"request_id"`
						Response  struct {
							Behavior     string         `json:"behavior"`
							UpdatedInput map[string]any `json:"updatedInput"`
						} `json:"response"`
					} `json:"response"`
				}
				scanned := input.Scan()
				controlLine := append([]byte(nil), input.Bytes()...)
				decodeErr := json.Unmarshal(controlLine, &response)
				if !scanned || decodeErr != nil || response.Type != "control_response" || response.Response.RequestID != "native-control-7" || response.Response.Response.Behavior != "allow" || response.Response.Response.UpdatedInput["file_path"] != "fixture.txt" {
					return 29
				}
			}
			stream := func(eventID string, event any) bool {
				return send(map[string]any{"type": "stream_event", "session_id": sessionID, "uuid": eventID, "event": event})
			}
			if !stream("event-1", map[string]any{"type": "message_start", "message": map[string]any{"id": "assistant-message-9"}}) {
				return 30
			}
			if !stream("event-2", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}) {
				return 31
			}
			if !stream("event-3", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "hello "}}) {
				return 32
			}
			if !stream("event-4", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "world"}}) {
				return 33
			}
			if !send(map[string]any{"type": "assistant", "session_id": sessionID, "message": map[string]any{"id": "assistant-message-9", "content": []any{map[string]any{"type": "text", "text": "hello "}, map[string]any{"type": "text", "text": "world"}}}}) {
				return 34
			}
			if !stream("event-5", map[string]any{"type": "content_block_stop", "index": 0}) {
				return 35
			}
			if !stream("event-6", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu-native-3", "name": "Write", "input": map[string]any{}}}) {
				return 36
			}
			if !stream("event-7", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"file_path":`}}) {
				return 37
			}
			if !send(map[string]any{"type": "assistant", "session_id": sessionID, "message": map[string]any{"id": "assistant-message-9", "content": []any{map[string]any{"type": "tool_use", "id": "toolu-native-3", "name": "Write", "input": map[string]any{"file_path": "fixture.txt"}}}}}) {
				return 38
			}
			if !stream("event-8", map[string]any{"type": "message_stop"}) {
				return 39
			}
			if !send(map[string]any{"type": "result", "subtype": "success", "session_id": sessionID, "is_error": false, "result": "hello world"}) {
				return 40
			}
			return 0 // Deliberately exit immediately after the final buffered result.
		default:
			return 32
		}
	}
	return 0
}

func startFixture(t *testing.T, resume string, fixtureEnv ...string) (*Session, <-chan conversation.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	events := make(chan conversation.Event, 32)
	env := append(os.Environ(), "GORACE=atexit_sleep_ms=0", "AMUX_CLAUDE_FIXTURE=1")
	env = append(env, fixtureEnv...)
	session, err := Start(ctx, Config{Directory: t.TempDir(), ResumeID: resume, Command: []string{os.Args[0], "--"}, Env: env}, func(ctx context.Context, event conversation.Event) error {
		select {
		case events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, events
}

func TestNativeStreamJSONAndExactResume(t *testing.T) {
	session, events := startFixture(t, "saved-session")
	if session.ID() != "saved-session" {
		t.Fatalf("resume ID = %q", session.ID())
	}
	first := <-events
	if first.Kind != "session_ready" || first.SessionID != "saved-session" {
		t.Fatalf("first event = %+v", first)
	}
	status, err := session.Prompt(context.Background(), "hello")
	if err != nil || status != "success" {
		t.Fatalf("Prompt() = %q, %v", status, err)
	}
	deltaTexts := map[string]bool{}
	messageIDs := map[string]bool{}
	toolIDs := map[string]bool{}
	toolUpdate, snapshot, seenResult, seenInit := false, false, false, false
	blankContent := false
	for !seenResult {
		select {
		case event := <-events:
			switch event.Kind {
			case "text_delta":
				deltaTexts[event.Text] = true
				messageIDs[event.MessageID] = true
			case "message_snapshot":
				snapshot = event.MessageID == "assistant-message-9" && event.Text == "hello world"
				messageIDs[event.MessageID] = true
			case "tool_call":
				toolIDs[event.ToolCallID] = true
			case "tool_update":
				toolUpdate = event.ToolCallID == "toolu-native-3" && len(event.Data) != 0
			case "content":
				blankContent = true
			case "turn_completed":
				seenResult = event.Status == "success" && event.Text == "hello world"
			case "state":
				seenInit = seenInit || (event.Status == "init" && event.SessionID == "saved-session")
			}
		case <-time.After(time.Second):
			t.Fatal("terminal result was not delivered")
		}
	}
	if !deltaTexts["hello "] || !deltaTexts["world"] || !seenInit || !snapshot || len(messageIDs) != 1 || !toolUpdate || !toolIDs["toolu-native-3"] || blankContent {
		t.Fatalf("stream mapping deltas=%v messages=%v toolIDs=%v update=%v snapshot=%v init=%v blankContent=%v", deltaTexts, messageIDs, toolIDs, toolUpdate, snapshot, seenInit, blankContent)
	}
	select {
	case <-session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not finish after buffered result and EOF")
	}
	if err := session.Err(); err != nil {
		t.Fatalf("reader error = %v", err)
	}
}

func TestResumeSessionIdentityMismatchFails(t *testing.T) {
	session, _ := startFixture(t, "saved-session", "AMUX_CLAUDE_MISMATCH=1")
	_, err := session.Prompt(context.Background(), "must not continue")
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("Prompt() = %v; expected exact-resume identity failure", err)
	}
}

func TestNativePermissionControlUsesExplicitDecision(t *testing.T) {
	session, events := startFixture(t, "")
	if _, err := uuid.Parse(session.ID()); err != nil {
		t.Fatalf("new session ID is not a UUID: %q", session.ID())
	}
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(context.Background(), "permission"); done <- err }()
	permissionEvent := conversation.Event{}
	for permissionEvent.Kind != "permission" {
		select {
		case event := <-events:
			permissionEvent = event
		case <-time.After(time.Second):
			t.Fatal("permission request was not surfaced")
		}
	}
	if permissionEvent.RequestID != "native-control-7" || permissionEvent.ToolCallID != "tool-native-2" || permissionEvent.Title != "Write" || !strings.Contains(string(permissionEvent.Data), "fixture.txt") || len(permissionEvent.Options) != 2 {
		t.Fatalf("permission = %+v", permissionEvent)
	}
	if err := session.ResolvePermission(context.Background(), permissionEvent.RequestID, "invented"); err == nil {
		t.Fatal("accepted unoffered permission choice")
	}
	if err := session.ResolvePermission(context.Background(), permissionEvent.RequestID, "allow-once"); err != nil {
		t.Fatalf("ResolvePermission() = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Prompt() = %v", err)
	}
	if err := session.ResolvePermission(context.Background(), permissionEvent.RequestID, "allow-once"); err == nil {
		t.Fatal("permission remained pending after turn completion")
	}
}

func TestNativePromptCancellation(t *testing.T) {
	// The fixture acknowledges interrupt and emits its terminal result.
	session, events := startFixture(t, "saved-session")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(ctx, "cancel"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind == "state" && event.Status == "init" {
				cancel()
				goto canceled
			}
		case <-time.After(time.Second):
			t.Fatal("fixture did not initialize prompt")
		}
	}
canceled:
	err := <-done
	if err == nil {
		t.Fatal("cancelled prompt returned without cancellation error")
	}
	if !strings.Contains(fmt.Sprint(err), "context canceled") {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestOversizedNativeFrameIsRejected(t *testing.T) {
	session, _ := startFixture(t, "", "AMUX_CLAUDE_OVERSIZE=1")
	select {
	case <-session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not stop on oversized frame")
	}
	if err := session.Err(); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("reader error = %v", err)
	}
}

func TestCancelUnblocksStalledControlResponseWriter(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	events := make(chan conversation.Event, 16)
	session, err := Start(parent, Config{Directory: t.TempDir(), Command: []string{os.Args[0], "--"}, Env: append(os.Environ(), "GORACE=atexit_sleep_ms=0", "AMUX_CLAUDE_FIXTURE=1", "AMUX_CLAUDE_STALLED_CONTROL=1")}, func(_ context.Context, event conversation.Event) error {
		events <- event
		return nil
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = events
	t.Cleanup(func() { cancel(); _ = session.Close() })
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-session.Done():
	case <-time.After(7 * time.Second):
		t.Fatal("reader remained blocked after provider cancellation")
	}
}

func TestUnsupportedControlReceivesExplicitError(t *testing.T) {
	session, events := startFixture(t, "", "AMUX_CLAUDE_UNSUPPORTED_CONTROL=1")
	answered := false
	for !answered {
		select {
		case event := <-events:
			answered = event.Kind == "state" && event.Status == "unsupported_control_answered"
		case <-time.After(3 * time.Second):
			t.Fatal("unsupported control was not answered")
		}
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := session.Err(); err != nil {
		t.Fatalf("reader error = %v", err)
	}
}
