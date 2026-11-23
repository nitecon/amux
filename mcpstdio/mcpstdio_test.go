package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

var testConfig = Config{Tools: []Tool{{Name: "echo", Description: "Echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}}

func TestLifecycleAndToolCall(t *testing.T) {
	var called bool
	config := testConfig
	config.Call = func(_ context.Context, name string, args json.RawMessage) (Result, error) {
		called = name == "echo" && string(args) == `{"value":"hi"}`
		return Result{Content: []TextContent{{Type: "text", Text: "hi"}}}, nil
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_meta":{"traceId":"t1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"value":"hi"}}}`,
	}, "\n") + "\n"
	out := runServe(t, strings.NewReader(input), config)
	lines := splitLines(t, out)
	if len(lines) != 3 {
		t.Fatalf("got %d responses, want 3: %s", len(lines), out)
	}
	var init struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	decodeLine(t, lines[0], &init)
	if string(init.ID) != "1" || init.Result.ProtocolVersion != latestVersion {
		t.Fatalf("unexpected initialize response: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"echo"`) || !strings.Contains(lines[2], `"text":"hi"`) || !called {
		t.Fatalf("tools were not listed/called: %s", out)
	}
}

func TestInitializeIncludesOnlyConfiguredInstructions(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n"
	config := Config{Instructions: "physical session guidance"}
	var response struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(runServe(t, strings.NewReader(input), config))), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.Instructions != config.Instructions {
		t.Fatalf("initialize instructions=%q", response.Result.Instructions)
	}
	if strings.Contains(runServe(t, strings.NewReader(input), Config{}), `"instructions"`) {
		t.Fatal("initialize emitted instructions without configuration")
	}
}

func TestDynamicToolCatalogIsRecheckedForListAndCall(t *testing.T) {
	availableChecks := 0
	calls := 0
	config := Config{
		ListTools: func() []Tool {
			availableChecks++
			if availableChecks <= 2 {
				return testConfig.Tools
			}
			return nil
		},
		Call: func(_ context.Context, name string, _ json.RawMessage) (Result, error) {
			if name != "echo" {
				t.Fatalf("unexpected tool call %q", name)
			}
			calls++
			return Result{Content: []TextContent{{Type: "text", Text: "ok"}}}, nil
		},
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
	}, "\n") + "\n"
	lines := splitLines(t, runServe(t, strings.NewReader(input), config))
	if len(lines) != 5 || calls != 1 || availableChecks != 4 {
		t.Fatalf("dynamic catalog checks=%d calls=%d responses=%d: %s", availableChecks, calls, len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], `"echo"`) || !strings.Contains(lines[2], `"text":"ok"`) ||
		!strings.Contains(lines[3], `"tools":[]`) || !strings.Contains(lines[4], `"unknown tool: echo"`) {
		t.Fatalf("dynamic tool availability was not applied to list and call: %s", strings.Join(lines, "\n"))
	}
}

func TestUnsupportedVersionUsesSupportedLatest(t *testing.T) {
	out := runServe(t, strings.NewReader(`{"jsonrpc":"2.0","id":"x","method":"initialize","params":{"protocolVersion":"2099-01-01"}}`+"\n"), Config{})
	if !strings.Contains(out, `"protocolVersion":"2025-11-25"`) || strings.Contains(out, "2099-01-01") {
		t.Fatalf("unsupported version was echoed: %s", out)
	}
}

func TestSupportedVersionsAreEchoed(t *testing.T) {
	for _, version := range supportedVersions {
		t.Run(version, func(t *testing.T) {
			input := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q}}`, version) + "\n"
			out := runServe(t, strings.NewReader(input), Config{})
			if !strings.Contains(out, `"protocolVersion":"`+version+`"`) {
				t.Fatalf("supported version not negotiated: %s", out)
			}
		})
	}
}

func TestDuplicateInitializeUnknownMethodAndTool(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":3,"method":"missing"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"missing","arguments":{}}}`,
	}, "\n") + "\n"
	lines := splitLines(t, runServe(t, strings.NewReader(input), testConfig))
	if len(lines) != 4 {
		t.Fatalf("got %d responses, want 4", len(lines))
	}
	for i, want := range []string{"initialize already completed", "method not found", "unknown tool"} {
		if !strings.Contains(lines[i+1], want) {
			t.Errorf("response %d missing %q: %s", i+1, want, lines[i+1])
		}
	}
}

func TestMalformedOversizeAndBatch(t *testing.T) {
	input := "{bad json}\n" + strings.Repeat("x", maxFrameBytes+1) + "\n" +
		"[]\n" + `{"jsonrpc":"2.0","id":5,"method":"ping"}` + "\n"
	lines := splitLines(t, runServe(t, strings.NewReader(input), Config{}))
	if len(lines) != 4 {
		t.Fatalf("got %d responses, want 4", len(lines))
	}
	if !strings.Contains(lines[0], `"code":-32700`) || !strings.Contains(lines[1], "frame exceeds 4 MiB") ||
		!strings.Contains(lines[2], `"code":-32600`) || !strings.Contains(lines[3], `"id":5`) {
		t.Fatalf("unexpected errors or recovery: %s", strings.Join(lines, "\n"))
	}
}

func TestFrameAbove1MiBWithin4MiBIsAccepted(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":19,"method":"ping","params":{"padding":"` + strings.Repeat("x", 2<<20) + `"}}`
	if len(frame) <= 1<<20 || len(frame) > maxFrameBytes {
		t.Fatalf("fixture frame size = %d, must be between 1 and 4 MiB", len(frame))
	}
	out := runServe(t, strings.NewReader(frame+"\n"), Config{})
	if !strings.Contains(out, `"id":19`) || strings.Contains(out, "frame exceeds") {
		t.Fatalf("valid frame above 1 MiB was rejected: %s", out)
	}
}

func TestInvalidParamsAndIDRoundTrip(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"request-id","method":"ping"}`,
		`{"jsonrpc":"2.0","id":123456789012345678901234567890,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
	}, "\n") + "\n"
	lines := splitLines(t, runServe(t, strings.NewReader(input), Config{}))
	if len(lines) != 3 || !strings.Contains(lines[0], `"id":"request-id"`) ||
		!strings.Contains(lines[1], `123456789012345678901234567890`) || !strings.Contains(lines[2], `"code":-32600`) {
		t.Fatalf("IDs were not preserved or null was accepted: %s", strings.Join(lines, "\n"))
	}

	beforeReady := `{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n"
	if out := runServe(t, strings.NewReader(beforeReady), Config{}); !strings.Contains(out, `"code":-32002`) {
		t.Fatalf("pre-initialization request was accepted: %s", out)
	}
	badParams := `{"jsonrpc":"2.0","id":8,"method":"initialize","params":[]}` + "\n"
	if out := runServe(t, strings.NewReader(badParams), Config{}); !strings.Contains(out, `"code":-32602`) {
		t.Fatalf("invalid params not reported: %s", out)
	}
}

func TestToolErrorsAreResults(t *testing.T) {
	config := testConfig
	config.Call = func(context.Context, string, json.RawMessage) (Result, error) {
		return Result{}, errors.New("backend failed")
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}` + "\n"
	out := runServe(t, strings.NewReader(input), config)
	if !strings.Contains(out, `"isError":true`) || !strings.Contains(out, "backend failed") || strings.Contains(out, `"code":-32603`) {
		t.Fatalf("callback failure was not returned as a tool error: %s", out)
	}
}

func TestToolOutputAboveFiveMiBRoundTripsWithoutTruncation(t *testing.T) {
	body := strings.Repeat("x", (5<<20)+256)
	config := testConfig
	config.Call = func(context.Context, string, json.RawMessage) (Result, error) {
		return Result{Content: []TextContent{{Type: "text", Text: body}}}, nil
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}` + "\n"
	out := runServe(t, strings.NewReader(input), config)
	lines := bytes.Split(bytes.TrimSuffix([]byte(out), []byte("\n")), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("large output response count=%d, want initialize and one tool response", len(lines))
	}
	if len(lines[1]) <= 5<<20 {
		t.Fatalf("tool response size=%d, want above 5 MiB", len(lines[1]))
	}
	var response struct {
		ID     json.RawMessage `json:"id"`
		Result Result          `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &response); err != nil {
		t.Fatal(err)
	}
	if string(response.ID) != "2" || len(response.Result.Content) != 1 {
		t.Fatalf("large output response id/content count=%s/%d, want 2/1", response.ID, len(response.Result.Content))
	}
	if response.Result.Content[0].Text != body {
		t.Fatalf("large output body length=%d, want %d", len(response.Result.Content[0].Text), len(body))
	}
}

func TestServePropagatesOutputWriterError(t *testing.T) {
	want := errors.New("writer failed")
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	err := Serve(context.Background(), input, errorWriter{err: want}, Config{})
	if !errors.Is(err, want) {
		t.Fatalf("Serve error=%v, want %v", err, want)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestMissingToolArgumentsAreEmptyObject(t *testing.T) {
	config := testConfig
	var got string
	config.Call = func(_ context.Context, _ string, args json.RawMessage) (Result, error) {
		got = string(args)
		return Result{}, nil
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}` + "\n"
	runServe(t, strings.NewReader(input), config)
	if got != `{}` {
		t.Fatalf("missing arguments passed as %q, want {}", got)
	}
}

func TestCallbackReceivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	config := testConfig
	config.Call = func(ctx context.Context, _ string, _ json.RawMessage) (Result, error) {
		close(entered)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}` + "\n"
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, strings.NewReader(input), io.Discard, config) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve returned %v, want context.Canceled", err)
	}
}

func TestCancellationUnblocksPipeOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, input, pipeWriter, Config{}) }()
	firstByte := make([]byte, 1)
	if _, err := pipeReader.Read(firstByte); err != nil {
		t.Fatalf("waiting for response output: %v", err)
	}
	// The pipe writer cannot finish the full frame until more bytes are read.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve remained blocked writing after cancellation")
	}
}

func TestEOFReturnsCleanly(t *testing.T) {
	if err := Serve(context.Background(), strings.NewReader(""), io.Discard, Config{}); err != nil {
		t.Fatalf("Serve at EOF: %v", err)
	}
}

func runServe(t *testing.T, input io.Reader, config Config) string {
	t.Helper()
	var out bytes.Buffer
	if err := Serve(context.Background(), input, &out, config); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return out.String()
}

func splitLines(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func decodeLine(t *testing.T, line string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(line), target); err != nil {
		t.Fatalf("invalid response %q: %v", line, err)
	}
}
