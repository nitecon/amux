package amuxruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

const claudeFixturePrompt = "Inspect this headless Claude task"

// TestNativeClaudeWorkerFixture is the fake Claude CLI used by the worker
// round-trip test. The same test binary accepts Claude's appended CLI flags.
func TestNativeClaudeWorkerFixture(t *testing.T) {
	if os.Getenv("AMUX_CLAUDE_WORKER_FIXTURE") != "1" {
		return
	}
	os.Exit(runClaudeWorkerFixture())
}

func runClaudeWorkerFixture() int {
	for _, name := range []string{"DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS"} {
		if os.Getenv(name) != "" {
			return 40
		}
	}
	if os.Getenv("HOME") != "" {
		return 41
	}
	args := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	argValue := func(name string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == name {
				return args[i+1]
			}
		}
		return ""
	}
	if argValue("--permission-prompts") != "host" || argValue("--input-format") != "stream-json" || argValue("--output-format") != "stream-json" {
		return 42
	}
	sessionID := argValue("--resume")
	resumed := sessionID != ""
	if resumed {
		if argValue("--session-id") != "" {
			return 43
		}
		saved, err := os.ReadFile("claude-session")
		if err != nil || string(saved) != sessionID {
			return 44
		}
	} else {
		sessionID = argValue("--session-id")
		if _, err := uuid.Parse(sessionID); err != nil {
			return 45
		}
		if err := os.WriteFile("claude-session", []byte(sessionID), 0600); err != nil {
			return 46
		}
	}

	input := bufio.NewScanner(os.Stdin)
	output := json.NewEncoder(os.Stdout)
	send := func(value any) bool { return output.Encode(value) == nil }
	initialized := false
	promptSeen := false
	for input.Scan() {
		var frame struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Message   struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(input.Bytes(), &frame) != nil {
			return 47
		}
		switch frame.Type {
		case "control_request":
			var request struct {
				Subtype string `json:"subtype"`
			}
			if json.Unmarshal(frame.Request, &request) != nil || request.Subtype != "initialize" || initialized {
				return 48
			}
			initialized = true
			if !send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": frame.RequestID, "response": map[string]any{"commands": []any{}, "models": []any{}, "agents": []any{}, "account": nil}}}) {
				return 49
			}
		case "user":
			if !initialized || resumed || promptSeen || frame.Message.Role != "user" || frame.Message.Content != claudeFixturePrompt {
				return 50
			}
			promptSeen = true
			f, err := os.OpenFile("claude-prompts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return 51
			}
			_, writeErr := f.WriteString(frame.Message.Content + "\n")
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				return 52
			}
			// Claude may announce initialization after accepting the first prompt.
			if !send(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID}) {
				return 53
			}
			if !send(map[string]any{"type": "stream_event", "session_id": sessionID, "uuid": "message-1", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "Claude fixture partial response"}}}) {
				return 54
			}
			if !send(map[string]any{"type": "control_request", "request_id": "fixture-permission-1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "tool_use_id": "tool-use-1", "title": "Write fixture output", "input": map[string]any{"file_path": "fixture.txt"}}}) {
				return 55
			}
			if !input.Scan() {
				return 56
			}
			var response struct {
				Type     string `json:"type"`
				Response struct {
					Subtype   string `json:"subtype"`
					RequestID string `json:"request_id"`
					Response  struct {
						Behavior     string         `json:"behavior"`
						UpdatedInput map[string]any `json:"updatedInput"`
					} `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(input.Bytes(), &response) != nil || response.Type != "control_response" || response.Response.Subtype != "success" || response.Response.RequestID != "fixture-permission-1" || response.Response.Response.Behavior != "allow" || response.Response.Response.UpdatedInput["file_path"] != "fixture.txt" {
				return 57
			}
			if !send(map[string]any{"type": "result", "subtype": "success", "session_id": sessionID, "is_error": false, "result": "Claude fixture completed"}) {
				return 58
			}
		default:
			return 59
		}
	}
	if err := input.Err(); err != nil {
		return 60
	}
	if !resumed && !promptSeen {
		return 61
	}
	marker := "claude-eof-fresh"
	if resumed {
		marker = "claude-eof-resumed"
	}
	if err := os.WriteFile(marker, []byte(sessionID), 0600); err != nil {
		return 62
	}
	return 0
}

func TestHeadlessClaudeWorkerPermissionRestartRoundTrip(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX signal smoke test; Windows service lifecycle is separate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err = remote.InsertProject(ctx, storage.Project{Ident: "claude-headless", ChannelName: "claude-headless", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "claude-headless", Title: "Claude headless", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "claude-secret")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	client, err := gatewayclient.New(srv.URL, "claude-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "worker.db")
	checkout := t.TempDir()
	agent, err := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeClaudeWorkerFixture$", "--"})
	if err != nil {
		t.Fatal(err)
	}
	start := func(args ...string) *workerProcess {
		return startWorkerProcess(t, ctx, state, map[string]string{"AMUX_CLAUDE_WORKER_FIXTURE": "1", "GATEWAY_API_KEY": "claude-secret", "HOME": ""}, args...)
	}
	process := start("--server", srv.URL, "--project", "claude-headless", "--task", task.ID, "--directory", checkout, "--provider", "claude", "--agent-command", string(agent), "--prompt", claudeFixturePrompt)
	if process.runID == "" || process.conversationID == "" {
		t.Fatalf("worker process did not return durable IDs: %+v", process)
	}

	var sawPermission, sawInitAfterPrompt bool
	for !sawPermission {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "claude-headless", process.conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var promptSequence, initSequence int64
		for _, event := range events {
			switch event.Event.Kind {
			case "user_message":
				if event.Event.Text != claudeFixturePrompt {
					t.Fatalf("persisted unexpected prompt %q", event.Event.Text)
				}
				promptSequence = event.Sequence
			case "state":
				if event.Event.Status == "init" {
					initSequence = event.Sequence
				}
			case "permission":
				if event.Event.RequestID != "fixture-permission-1" || event.Event.ToolCallID != "tool-use-1" || event.Event.Title != "Write fixture output" || len(event.Event.Options) != 2 {
					t.Fatalf("unexpected Claude permission event: %+v", event.Event)
				}
				if event.Event.Options[0].ID != "allow-once" || event.Event.Options[1].ID != "deny" {
					t.Fatalf("unexpected permission choices: %+v", event.Event.Options)
				}
				if err = client.Enqueue(ctx, "claude-headless", process.conversationID, uuid.NewString(), conversation.Command{Kind: "permission", PermissionEventID: event.ID, OptionID: "allow-once"}); err != nil {
					t.Fatal(err)
				}
				sawPermission = true
			}
		}
		sawInitAfterPrompt = promptSequence > 0 && initSequence > promptSequence
		if !sawPermission {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatalf("Claude permission did not arrive: %s", process.stderr)
			}
		}
	}
	if !sawInitAfterPrompt {
		t.Fatal("Claude init event was not persisted after the first user prompt")
	}

	for {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "claude-headless", process.conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var text, result bool
		for _, event := range events {
			text = text || event.Event.Kind == "text_delta" && event.Event.Text == "Claude fixture partial response"
			result = result || event.Event.Kind == "turn_completed" && event.Event.Status == "success" && event.Event.Text == "Claude fixture completed"
		}
		commands, err := remote.ConversationCommands(ctx, "claude-headless", process.conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		allDone := len(commands) == 2
		for _, command := range commands {
			allDone = allDone && command.State == "done"
		}
		if text && result && allDone {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("Claude output/result or command persistence did not complete: %s", process.stderr)
		}
	}
	stopWorkerProcess(t, ctx, process)
	if data, err := os.ReadFile(filepath.Join(checkout, "claude-eof-fresh")); err != nil || len(data) == 0 {
		t.Fatalf("Claude did not shut down on worker stdin EOF: %q %v", data, err)
	}

	resumed := start("--run-id", process.runID)
	if resumed.runID != process.runID || resumed.conversationID != process.conversationID {
		t.Fatal("restart replaced the durable worker or conversation ID")
	}
	for {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "claude-headless", process.conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		ready := 0
		for _, event := range events {
			if event.Event.Kind == "session_ready" {
				ready++
			}
		}
		if ready == 2 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("Claude did not reopen the saved native session: %s", resumed.stderr)
		}
	}
	stopWorkerProcess(t, ctx, resumed)
	if data, err := os.ReadFile(filepath.Join(checkout, "claude-eof-resumed")); err != nil || len(data) == 0 {
		t.Fatalf("resumed Claude process did not receive EOF: %q %v", data, err)
	}
	if session, err := os.ReadFile(filepath.Join(checkout, "claude-session")); err != nil || len(session) == 0 {
		t.Fatalf("Claude fixture did not retain its session ID: %q %v", session, err)
	}
	prompts, err := os.ReadFile(filepath.Join(checkout, "claude-prompts"))
	if err != nil || string(prompts) != claudeFixturePrompt+"\n" {
		t.Fatalf("completed prompt repeated on restart: %q %v", prompts, err)
	}
	events, err := remote.ConversationEvents(ctx, "claude-headless", process.conversationID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var resultCount int
	for _, event := range events {
		if event.Event.Kind == "turn_completed" && event.Event.Status == "success" {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Fatalf("expected one persisted result across restart, got %d", resultCount)
	}
}
