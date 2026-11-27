package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestMessagesCLIProcessReadsThreadAndRefusesUnwrappedPublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "messages-cli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "internal", RoomID: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	message, err := store.CreateHumanProjectMessage(ctx, "demo", storage.HumanProjectMessageInput{Content: "inline handoff"})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "synthetic-cli-key")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Agent-Session-Id") != "" {
			t.Error("readonly CLI attached native identity")
		}
		handler.ServeHTTP(w, r)
	}))
	defer host.Close()
	invoke := func(operation ...string) ([]byte, string, error) {
		t.Helper()
		// The existing TestMain dispatcher invokes main directly, keeping the
		// test runner's PASS footer out of the CLI's JSON output.
		args := []string{"messages", "--gateway-url", host.URL, "--project", "demo"}
		command := exec.CommandContext(ctx, os.Args[0], append(args, operation...)...)
		command.Env = workerTestEnvironment(t, map[string]string{
			"AMUX_MESSAGES_CLI_PROCESS_TEST": "1", "GATEWAY_API_KEY": "synthetic-cli-key",
			"AMUX_AGENT_SESSION_ID": "", "AMUX_AGENT_INSTANCE_ID": "", "AMUX_AGENT_PROVIDER": "", "AMUX_AGENT_OS": "",
			"AMUX_AGENT_PROJECT": "", "AMUX_GATEWAY_API_KEY_ENV": "", "AMUX_GATEWAY_KEY": "",
		})
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		return output, stderr.String(), err
	}
	output, stderr, err := invoke("get", strconv.FormatInt(message.ID, 10))
	if err != nil {
		t.Fatalf("shipped CLI thread read: %v stderr=%s", err, stderr)
	}
	var thread storage.ProjectMessageThread
	if err := json.Unmarshal(output, &thread); err != nil || thread.Root.ID != message.ID || thread.Root.Content != "inline handoff" {
		t.Fatalf("CLI thread output=%q err=%v", output, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("thread read made %d requests", requests.Load())
	}
	output, stderr, err = invoke("send", "must not publish")
	if err == nil || !strings.Contains(stderr, "native identity") || len(output) != 0 || requests.Load() != 1 {
		t.Fatalf("unwrapped publication was not refused locally: err=%v output=%q stderr=%q requests=%d", err, output, stderr, requests.Load())
	}
}
