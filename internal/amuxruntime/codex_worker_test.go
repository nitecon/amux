package amuxruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

// TestNativeCodexWorkerFixture is launched as the native app-server process by
// the headless worker smoke test. It requires neither Codex login nor a model.
func TestNativeCodexWorkerFixture(t *testing.T) {
	if os.Getenv("AMUX_CODEX_WORKER_FIXTURE") != "1" {
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
	threadID := "codex-worker-thread"
	initialized := false
	for scan.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil || req.JSONRPC != "" {
			os.Exit(12)
		}
		respond := func(result any) { send(map[string]any{"id": req.ID, "result": result}) }
		switch req.Method {
		case "initialize":
			respond(map[string]any{"userAgent": "fixture"})
		case "initialized":
			initialized = true
		case "thread/start", "thread/resume":
			if !initialized {
				os.Exit(13)
			}
			var params struct {
				ID string `json:"threadId"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if req.Method == "thread/resume" {
				if params.ID != threadID {
					os.Exit(14)
				}
			} else if params.ID != "" {
				os.Exit(15)
			}
			send(map[string]any{"method": "thread/started", "params": map[string]any{"thread": map[string]any{"id": threadID}}})
			respond(map[string]any{"thread": map[string]any{"id": threadID, "status": map[string]any{"type": "idle"}, "turns": []any{}}})
		case "turn/start":
			var params struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.ThreadID != threadID || len(params.Input) != 1 || params.Input[0].Text != "Inspect the headless task" {
				os.Exit(16)
			}
			f, err := os.OpenFile("codex-prompts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(17)
			}
			_, _ = f.WriteString(params.Input[0].Text + "\n")
			_ = f.Close()
			if os.Getenv("AMUX_CODEX_WORKER_MESSAGES_FIXTURE") == "1" {
				if err := runNativeMessagesWorkerCLI(); err != nil {
					if path := os.Getenv("AMUX_MESSAGES_WORKER_ERROR"); path != "" {
						_ = os.WriteFile(path, []byte(err.Error()), 0600)
					}
					os.Exit(21)
				}
			}
			send(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}}})
			respond(map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}})
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": threadID, "turnId": "turn-1", "itemId": "message-1", "delta": "Headless Codex is ready"}})
			send(map[string]any{"id": "command-approval", "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": threadID, "turnId": "turn-1", "itemId": "command-1", "command": "git status", "availableDecisions": []string{"accept", "decline"}}})
		case "":
			var decision struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal(req.Result, &decision)
			if decision.Decision != "accept" {
				os.Exit(18)
			}
			switch string(req.ID) {
			case `"command-approval"`:
				send(map[string]any{"id": "file-approval", "method": "item/fileChange/requestApproval", "params": map[string]any{"threadId": threadID, "turnId": "turn-1", "itemId": "file-1", "reason": "Apply requested edit", "availableDecisions": []string{"accept", "decline"}}})
			case `"file-approval"`:
				send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}}})
			default:
				os.Exit(19)
			}
		case "turn/interrupt":
			respond(map[string]any{})
		default:
			os.Exit(20)
		}
	}
	_ = os.WriteFile("codex-eof", []byte("EOF"), 0600)
	os.Exit(0)
}

func TestHeadlessCodexWorkerPermissionRestartRoundTrip(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX signal smoke test; Windows service lifecycle is separate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err = remote.InsertProject(ctx, storage.Project{Ident: "codex-headless", ChannelName: "codex-headless", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "codex-headless", Title: "Codex headless", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "codex-secret")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	client, err := gatewayclient.New(srv.URL, "codex-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "worker.db")
	checkout := t.TempDir()
	agent, _ := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeCodexWorkerFixture$"})
	start := func(args ...string) *workerProcess {
		return startWorkerProcess(t, ctx, state, map[string]string{"AMUX_CODEX_WORKER_FIXTURE": "1", "GATEWAY_API_KEY": "codex-secret"}, args...)
	}
	cmd := start("--server", srv.URL, "--project", "codex-headless", "--task", task.ID, "--directory", checkout, "--provider", "codex", "--agent-command", string(agent), "--prompt", "Inspect the headless task")
	runID, conversationID, diagnostics := cmd.runID, cmd.conversationID, cmd.stderr
	answered := map[string]bool{}
	var sawText, sawTurnStart bool
	var sawCommandApproval, sawFileApproval bool
	for len(answered) < 2 {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "codex-headless", conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			switch event.Event.Kind {
			case "permission":
				if answered[event.ID] {
					continue
				}
				if strings.Contains(event.Event.Title, "git status") {
					sawCommandApproval = true
				} else if strings.Contains(event.Event.Title, "Apply requested edit") {
					sawFileApproval = true
				}
				if len(answered) == 0 {
					commands, err := remote.ConversationCommands(ctx, "codex-headless", conversationID, 0, 100)
					if err != nil {
						t.Fatal(err)
					}
					if len(commands) == 0 || commands[0].State != "claimed" {
						t.Fatalf("turn command was not claimed while approval was pending: %+v", commands)
					}
				}
				if err = client.Enqueue(ctx, "codex-headless", conversationID, uuid.NewString(), conversation.Command{Kind: "permission", PermissionEventID: event.ID, OptionID: "accept"}); err != nil {
					t.Fatal(err)
				}
				answered[event.ID] = true
			case "text_delta":
				sawText = sawText || event.Event.Text == "Headless Codex is ready"
			case "turn_started":
				sawTurnStart = true
			}
		}
		if len(answered) < 2 {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatalf("Codex did not request both approvals: %s", diagnostics)
			}
		}
	}
	if !sawText || !sawTurnStart || !sawCommandApproval || !sawFileApproval {
		t.Fatalf("missing Codex protocol events: text=%v started=%v commandApproval=%v fileApproval=%v", sawText, sawTurnStart, sawCommandApproval, sawFileApproval)
	}
	for {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "codex-headless", conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		completed := false
		for _, event := range events {
			completed = completed || event.Event.Kind == "turn_completed" && event.Event.Status == "completed"
		}
		commands, err := remote.ConversationCommands(ctx, "codex-headless", conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		allDone := len(commands) == 3
		for _, command := range commands {
			allDone = allDone && command.State == "done"
		}
		if completed && allDone {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("Codex turn or commands did not complete: %s", diagnostics)
		}
	}
	stopWorkerProcess(t, ctx, cmd)
	if _, err = os.Stat(filepath.Join(checkout, "codex-eof")); err != nil {
		t.Fatal("Codex app-server did not shut down on stdin EOF")
	}
	var output bytes.Buffer
	err = runWorker(ctx, []string{"--state", state, "--run-id", runID, "--provider", "antigravity"}, &output, io.Discard, func(name string) string {
		if name == "GATEWAY_API_KEY" {
			return "codex-secret"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "saved worker uses provider \"codex\"") {
		t.Fatalf("explicit provider override was not rejected: %v", err)
	}
	resumed := start("--run-id", runID)
	if resumed.runID != runID || resumed.conversationID != conversationID {
		t.Fatal("resume replaced saved worker identity")
	}
	for {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "codex-headless", conversationID, 0, 100)
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
			t.Fatalf("Codex worker did not resume: %s", resumed.stderr)
		}
	}
	stopWorkerProcess(t, ctx, resumed)
	prompts, err := os.ReadFile(filepath.Join(checkout, "codex-prompts"))
	if err != nil || string(prompts) != "Inspect the headless task\n" {
		t.Fatalf("completed Codex prompt repeated after restart: %q %v", prompts, err)
	}
}
