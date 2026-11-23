package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nitecon/amux/conversation"
)

func TestNativeCodexFixture(t *testing.T) {
	if os.Getenv("AMUX_CODEX_FIXTURE") != "1" {
		return
	}
	info, _ := os.Stdin.Stat()
	if info.Mode()&os.ModeCharDevice != 0 {
		os.Exit(10)
	}
	scan := bufio.NewScanner(os.Stdin)
	encode := json.NewEncoder(os.Stdout)
	send := func(v any) {
		if encode.Encode(v) != nil {
			os.Exit(11)
		}
	}
	initialized := false
	thread := "thread-native"
	turnNumber := 0
	turnID := ""
	turnCompleted := false
	for scan.Scan() {
		var req struct {
			Version string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil || req.Version != "" {
			os.Exit(12)
		}
		respond := func(value any) { send(map[string]any{"id": req.ID, "result": value}) }
		complete := func(status string) {
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": thread, "turn": map[string]any{"id": turnID, "status": status, "items": []any{}}}})
			turnCompleted = true
		}
		switch req.Method {
		case "initialize":
			respond(map[string]any{"userAgent": "fixture", "platformFamily": "unix", "platformOs": "linux", "codexHome": "/fixture"})
		case "initialized":
			initialized = true
		case "thread/start", "thread/resume":
			if !initialized {
				os.Exit(13)
			}
			if req.Method == "thread/resume" {
				var params struct {
					ID      string `json:"threadId"`
					Exclude bool   `json:"excludeTurns"`
				}
				json.Unmarshal(req.Params, &params)
				if params.ID != "saved-thread" || !params.Exclude {
					os.Exit(14)
				}
				thread = params.ID
				if os.Getenv("AMUX_CODEX_MISSING") == "1" {
					send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32600, "message": "thread missing"}})
					continue
				}
			}
			send(map[string]any{"method": "account/rateLimits/updated"})
			// Bootstrap notification precedes the RPC result; journal consumers still
			// need session_ready to be delivered first.
			send(map[string]any{"method": "thread/started", "params": map[string]any{"thread": map[string]any{"id": thread}}})
			respond(map[string]any{"thread": map[string]any{"id": thread, "status": map[string]any{"type": "idle"}, "turns": []any{}}})
		case "turn/start":
			var params struct {
				ID    string                        `json:"threadId"`
				Input []struct{ Type, Text string } `json:"input"`
			}
			json.Unmarshal(req.Params, &params)
			if params.ID != thread || len(params.Input) != 1 || params.Input[0].Type != "text" {
				os.Exit(15)
			}
			turnNumber++
			turnID = fmt.Sprintf("turn-%d", turnNumber)
			turnCompleted = false
			send(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": thread, "turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}}})
			respond(map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}})
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "foreign-thread", "turnId": turnID, "itemId": "foreign-message", "delta": "must not leak"}})
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": thread, "turnId": turnID, "itemId": fmt.Sprintf("message-%d", turnNumber), "delta": "Inspecting task"}})
			if params.Input[0].Text == "finish immediately" {
				complete("completed")
				os.Exit(0)
			}
			if params.Input[0].Text == "fail turn" {
				complete("failed")
				continue
			}
			if params.Input[0].Text == "wait for cancel" {
				continue
			}
			send(map[string]any{"method": "item/started", "params": map[string]any{"threadId": thread, "turnId": turnID, "item": map[string]any{"id": fmt.Sprintf("tool-%d", turnNumber), "type": "commandExecution", "command": "git status", "status": "inProgress"}}})
			method := "item/commandExecution/requestApproval"
			if params.Input[0].Text == "change files" {
				method = "item/fileChange/requestApproval"
			}
			approval := map[string]any{"threadId": thread, "turnId": turnID, "itemId": fmt.Sprintf("tool-%d", turnNumber), "command": "git status", "reason": "Review this command"}
			if params.Input[0].Text == "grant profile" {
				method = "item/permissions/requestApproval"
				approval["permissions"] = map[string]any{"fileSystem": map[string]any{"read": []string{"./"}}}
			}
			if params.Input[0].Text == "only accept" {
				approval["availableDecisions"] = []string{"accept"}
			}
			send(map[string]any{"id": fmt.Sprintf("approval-%d", turnNumber), "method": method, "params": approval})
		case "turn/interrupt":
			var params struct {
				ID   string `json:"threadId"`
				Turn string `json:"turnId"`
			}
			json.Unmarshal(req.Params, &params)
			if params.ID != thread || params.Turn != turnID {
				os.Exit(16)
			}
			if os.Getenv("AMUX_CODEX_INTERRUPT_ERROR") == "1" || turnCompleted {
				send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32602, "message": "turn is no longer active"}})
				continue
			}
			respond(map[string]any{})
			complete("interrupted")
		case "":
			var decision struct {
				Decision    string          `json:"decision"`
				Permissions json.RawMessage `json:"permissions"`
				Scope       string          `json:"scope"`
			}
			json.Unmarshal(req.Result, &decision)
			if len(decision.Permissions) > 0 {
				var profile map[string]any
				if json.Unmarshal(decision.Permissions, &profile) != nil || len(profile) == 0 || (decision.Scope != "turn" && decision.Scope != "session") {
					os.Exit(19)
				}
				complete("completed")
				continue
			}
			if decision.Decision == "cancel" {
				complete("interrupted")
				continue
			}
			if decision.Decision != "decline" && decision.Decision != "accept" && decision.Decision != "acceptForSession" {
				os.Exit(17)
			}
			if decision.Decision == "accept" || decision.Decision == "acceptForSession" {
				continue
			}
			send(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"threadId": thread, "requestId": req.ID}})
			send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": thread, "turnId": turnID, "item": map[string]any{"id": fmt.Sprintf("tool-%d", turnNumber), "type": "commandExecution", "command": "git status", "status": "declined"}}})
			complete("completed")
		default:
			os.Exit(18)
		}
	}
	os.Exit(0)
}
func nativeSession(t *testing.T, resume string, extraEnv ...string) (*Session, <-chan conversation.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	events := make(chan conversation.Event, 32)
	env := append(os.Environ(), "AMUX_CODEX_FIXTURE=1")
	env = append(env, extraEnv...)
	session, err := Start(ctx, Config{Directory: t.TempDir(), ResumeID: resume, Command: []string{os.Args[0], "-test.run=^TestNativeCodexFixture$"}, Env: env}, func(ctx context.Context, e conversation.Event) error {
		select {
		case events <- e:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	first := <-events
	if first.Kind != "session_ready" {
		t.Fatalf("bootstrap preceded binding: %+v", first)
	}
	return session, events
}
func TestCodexTurnWaitsForCompletionAndRoutesApproval(t *testing.T) {
	session, events := nativeSession(t, "")
	done := make(chan error, 1)
	go func() {
		status, err := session.Prompt(context.Background(), "Inspect task")
		if err == nil && status != "completed" {
			err = errors.New("wrong terminal status")
		}
		done <- err
	}()
	text, tool, permission, completed := false, false, false, false
	for !completed {
		select {
		case event := <-events:
			switch event.Kind {
			case "text_delta":
				if event.Text == "must not leak" {
					t.Fatal("foreign thread output crossed session boundary")
				}
				text = event.Text == "Inspecting task"
			case "tool_call":
				tool = event.ToolCallID == "tool-1"
			case "permission":
				permission = true
				select {
				case err := <-done:
					t.Fatalf("turn/start acknowledgement completed prompt: %v", err)
				default:
				}
				if err := session.ResolvePermission(context.Background(), event.RequestID, "invented"); err == nil {
					t.Fatal("unoffered approval accepted")
				}
				if err := session.ResolvePermission(context.Background(), event.RequestID, "decline"); err != nil {
					t.Fatal(err)
				}
			case "turn_completed":
				completed = true
			}
		case <-time.After(3 * time.Second):
			t.Fatal("native Codex stalled")
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !text || !tool || !permission {
		t.Fatalf("missing native events text=%v tool=%v approval=%v", text, tool, permission)
	}
}
func TestCodexExactResumeAndNativeInterrupt(t *testing.T) {
	session, events := nativeSession(t, "saved-thread")
	if session.ID() != "saved-thread" {
		t.Fatal("native thread replaced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(ctx, "wait for cancel"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind == "text_delta" {
				cancel()
				goto interrupted
			}
		case <-time.After(3 * time.Second):
			t.Fatal("turn never started")
		}
	}
interrupted:
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt was not acknowledged")
	}
}
func TestCodexFinalCompletionSurvivesEOF(t *testing.T) {
	session, _ := nativeSession(t, "")
	status, err := session.Prompt(context.Background(), "finish immediately")
	if err != nil || status != "completed" {
		t.Fatalf("lost final turn: %q %v", status, err)
	}
}

func TestCodexFileApprovalIsExplicit(t *testing.T) {
	session, events := nativeSession(t, "")
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(context.Background(), "change files"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind != "permission" {
				continue
			}
			if len(event.Options) == 0 {
				t.Fatal("file approval had no explicit choices")
			}
			if err := session.ResolvePermission(context.Background(), event.RequestID, "decline"); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("file approval did not arrive")
		}
	}
}

func TestCodexPermissionProfileGrantRequiresExplicitChoice(t *testing.T) {
	session, events := nativeSession(t, "")
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(context.Background(), "grant profile"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind != "permission" {
				continue
			}
			found := false
			for _, option := range event.Options {
				if option.ID == "grantTurn" {
					found = true
				}
			}
			if !found {
				t.Fatal("permission profile lacked explicit turn grant")
			}
			if err := session.ResolvePermission(context.Background(), event.RequestID, "grantTurn"); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("permission profile request did not arrive")
		}
	}
}

func TestCodexApprovalCancelCompletesAndAllowsNextTurn(t *testing.T) {
	session, events := nativeSession(t, "", "AMUX_CODEX_INTERRUPT_ERROR=1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(ctx, "Inspect task"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind != "permission" {
				continue
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled turn returned %v", err)
			}
			status, err := session.Prompt(context.Background(), "finish immediately")
			if err != nil || status != "completed" {
				t.Fatalf("session unhealthy after approval cancellation: status=%q err=%v", status, err)
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("approval did not arrive for cancellation")
		}
	}
}

func TestCodexFailedTurnReturnsError(t *testing.T) {
	session, _ := nativeSession(t, "")
	if status, err := session.Prompt(context.Background(), "fail turn"); err == nil || status != "failed" {
		t.Fatalf("failed turn was not surfaced: status=%q err=%v", status, err)
	}
}

func TestCodexApprovalWithoutSafeCancelFailsExplicitly(t *testing.T) {
	session, events := nativeSession(t, "")
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(context.Background(), "only accept"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind == "unsupported_request" {
				if err := <-done; err == nil {
					t.Fatal("unsupported approval policy was accepted")
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("unsupported approval policy did not fail explicitly")
		}
	}
}

func TestCodexMissingResumeFailsWithoutFreshThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := false
	session, err := Start(ctx, Config{Directory: t.TempDir(), ResumeID: "saved-thread", Command: []string{os.Args[0], "-test.run=^TestNativeCodexFixture$"}, Env: append(os.Environ(), "AMUX_CODEX_FIXTURE=1", "AMUX_CODEX_MISSING=1")}, func(context.Context, conversation.Event) error { ready = true; return nil })
	if err == nil {
		session.Close()
		t.Fatal("missing native history replaced silently")
	}
	if ready {
		t.Fatal("failed resume emitted a ready thread")
	}
}
