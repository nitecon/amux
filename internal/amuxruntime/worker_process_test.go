package amuxruntime

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type workerProcess struct {
	cmd            *exec.Cmd
	runID          string
	conversationID string
	poolID         string
	stderr         *synchronizedBuffer
	stdoutDone     <-chan error
}

func TestWorkerProcessHelper(t *testing.T) {
	if os.Getenv("AMUX_GO_WORKER_PROCESS_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := run(ctx, os.Args[i+1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	t.Fatal("worker process arguments missing")
}

func startWorkerProcess(t *testing.T, ctx context.Context, state string, env map[string]string, args ...string) *workerProcess {
	return startWorkerProcessMode(t, ctx, state, env, "run", args...)
}

func startWorkerProcessMode(t *testing.T, ctx context.Context, state string, env map[string]string, mode string, args ...string) *workerProcess {
	return startWorkerProcessModeWithTaskTools(t, ctx, state, env, mode, false, args...)
}

func startWorkerProcessModeWithTaskTools(t *testing.T, ctx context.Context, state string, env map[string]string, mode string, enableTaskTools bool, args ...string) *workerProcess {
	t.Helper()
	commandArgs := []string{"-test.run=^TestWorkerProcessHelper$", "--", "worker", mode, "--state", state}
	if !enableTaskTools {
		commandArgs = append(commandArgs, "--no-task-tools")
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], commandArgs...)
	cmd.Env = workerTestEnvironment(t, env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &synchronizedBuffer{}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	firstLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			firstLine <- scanner.Text()
		} else {
			firstLine <- ""
		}
		for scanner.Scan() {
		}
		done <- scanner.Err()
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	var line string
	select {
	case line = <-firstLine:
	case <-ctx.Done():
		t.Fatalf("worker did not report its durable run: %s", stderr)
	}
	fields := strings.Fields(line)
	if mode == "pool" {
		if len(fields) != 3 || fields[0] != "worker" || fields[1] != "pool" {
			t.Fatalf("pool startup output %q: %s", line, stderr)
		}
		return &workerProcess{cmd: cmd, poolID: fields[2], stderr: stderr, stdoutDone: done}
	}
	if len(fields) != 5 || fields[0] != "worker" || fields[1] != "run" || fields[3] != "conversation" {
		t.Fatalf("worker startup output %q: %s", line, stderr)
	}
	return &workerProcess{cmd: cmd, runID: fields[2], conversationID: fields[4], stderr: stderr, stdoutDone: done}
}

func stopWorkerProcess(t *testing.T, ctx context.Context, process *workerProcess) {
	t.Helper()
	if err := process.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal worker shutdown: %v; stderr: %s", err, process.stderr)
	}
	select {
	case err := <-process.stdoutDone:
		if err != nil {
			t.Fatalf("read worker stdout: %v; stderr: %s", err, process.stderr)
		}
	case <-ctx.Done():
		t.Fatalf("worker stdout did not close: %s", process.stderr)
	}
	if err := process.cmd.Wait(); err != nil {
		t.Fatalf("worker shutdown: %v; stderr: %s", err, process.stderr)
	}
}

func workerTestEnvironment(t *testing.T, overrides map[string]string) []string {
	t.Helper()
	values := map[string]string{
		"AMUX_GO_WORKER_PROCESS_TEST": "1",
		"DISPLAY":                     "",
		"WAYLAND_DISPLAY":             "",
		"DBUS_SESSION_BUS_ADDRESS":    "",
		"HOME":                        t.TempDir(),
	}
	for key, value := range overrides {
		values[key] = value
	}
	env := make([]string, 0, len(os.Environ())+len(values))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := values[key]; !replaced {
			env = append(env, item)
		}
	}
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}
