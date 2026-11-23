package providerprocess

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func TestProcessFixture(t *testing.T) {
	if os.Getenv("AMUX_PROCESS_FIXTURE") != "1" {
		return
	}
	switch os.Getenv("AMUX_PROCESS_MODE") {
	case "final":
		io.WriteString(os.Stdout, "final response\n")
		os.Exit(0)
	case "eof":
		io.Copy(io.Discard, os.Stdin)
		os.WriteFile("saved", []byte("session saved"), 0600)
		os.Exit(0)
	case "hang":
		io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Hour)
	}
	os.Exit(1)
}
func processFixture(t *testing.T, mode string, ctx context.Context) *Process {
	t.Helper()
	timeout := 5 * time.Second
	if mode == "hang" {
		timeout = 100 * time.Millisecond
	}
	p, err := Start(ctx, Options{Command: []string{os.Args[0], "-test.run=^TestProcessFixture$"}, Directory: t.TempDir(), Env: append(os.Environ(), "AMUX_PROCESS_FIXTURE=1", "AMUX_PROCESS_MODE="+mode), ShutdownTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}
func TestFinalOutputSurvivesProcessExit(t *testing.T) {
	p := processFixture(t, "final", context.Background())
	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not exit")
	}
	data, err := io.ReadAll(p)
	if err != nil || string(data) != "final response\n" {
		t.Fatalf("final output lost: %q, %v", data, err)
	}
}
func TestCloseLetsProviderSaveOnEOF(t *testing.T) {
	p := processFixture(t, "eof", context.Background())
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.command.Dir + string(os.PathSeparator) + "saved")
	if err != nil || string(data) != "session saved" {
		t.Fatalf("provider was killed before save: %q, %v", data, err)
	}
}
func TestCancellationRetiresUnresponsiveProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := processFixture(t, "hang", ctx)
	cancel()
	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("provider survived owner cancellation")
	}
}
