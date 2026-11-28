package amuxruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/providers/process"
)

func runMCP(t *testing.T, ctx context.Context, args []string, stdin io.ReadCloser, stdout, stderr io.Writer, getenv func(string) string) error {
	t.Helper()
	keys := []string{"GATEWAY_API_KEY", "AMUX_BUNDLED_DIR", "AMUX_AGENT_PROVIDER", "AMUX_WORK_SUBMISSION_PREFERENCES"}
	for _, key := range keys {
		if value := getenv(key); os.Getenv(key) != value {
			t.Setenv(key, value)
		}
	}
	return runMCPIO(ctx, args, stdin, stdout, stderr)
}

// Replay finite legacy test transcripts as a client that waits for each response
// before sending the next request or closing the connection.
func runMCPIO(ctx context.Context, args []string, stdin io.ReadCloser, stdout, stderr io.Writer) error {
	defer stdin.Close()
	requests, send, err := os.Pipe()
	if err != nil {
		return err
	}
	defer requests.Close()
	defer send.Close()
	replies, reply, err := os.Pipe()
	if err != nil {
		return err
	}
	defer replies.Close()
	defer reply.Close()
	var diagnostics bytes.Buffer
	cmd := exec.CommandContext(ctx, os.Getenv(mcpTestBinaryEnv), args...)
	cmd.Env = append(os.Environ(), process.ManagedMCPEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = requests, reply, &diagnostics
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = requests.Close()
	_ = reply.Close()
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = stdin.Close()
		done <- err
	}()
	reader, responses := bufio.NewReader(stdin), bufio.NewReader(replies)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				line = append(line, '\n')
			}
			if _, err := send.Write(line); err != nil {
				break
			}
			var request struct{ ID json.RawMessage }
			if json.Unmarshal(line, &request) == nil && len(request.ID) != 0 {
				for {
					response, err := responses.ReadBytes('\n')
					if len(response) != 0 {
						_, _ = stdout.Write(response)
					}
					if err != nil {
						break
					}
					var message struct{ ID json.RawMessage }
					if json.Unmarshal(response, &message) == nil && string(message.ID) == string(request.ID) {
						break
					}
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	_ = send.Close()
	err = <-done
	_, _ = stderr.Write(diagnostics.Bytes())
	if err != nil && diagnostics.Len() > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(diagnostics.String()))
	}
	return err
}

func mcpTestHome(t *testing.T, settings config.Client) string {
	t.Helper()
	if settings.APIKeyEnv == "" && settings.EnrolledProfile == "" {
		settings.APIKeyEnv = "GATEWAY_API_KEY"
	}
	home := t.TempDir()
	if err := config.SaveClient(filepath.Join(home, ".amux", "config.yaml"), settings); err != nil {
		t.Fatal(err)
	}
	return home
}

func writeMCPTestSettings(t *testing.T, settings config.Client) {
	t.Helper()
	home := mcpTestHome(t, settings)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

const mcpTestBinaryEnv = "AMUX_TEST_MCP_BINARY"

// Build the CLI once so the integration fixtures use the actual managed MCP
// entry point without duplicating its argument parsing.
func runTestsWithMCPBinary(m *testing.M) int {
	directory, err := os.MkdirTemp("", "amux-mcp-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(directory)
	binary := filepath.Join(directory, "amux.exe")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "../../cmd")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build MCP test binary:", err)
		return 1
	}
	if err := os.Setenv(mcpTestBinaryEnv, binary); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}
