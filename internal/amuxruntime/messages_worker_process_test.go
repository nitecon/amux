package amuxruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type nativeMessagesWorkerIdentity struct {
	SessionID  string `json:"session_id"`
	InstanceID string `json:"instance_id"`
	Provider   string `json:"provider"`
	OS         string `json:"os"`
	Directory  string `json:"directory"`
	Project    string `json:"project"`
	GatewayURL string `json:"gateway_url"`
	APIKeyEnv  string `json:"api_key_env"`
}

// This test is a CLI subprocess invoked by the synthetic native Codex fixture.
// It inherits the exact environment and working directory given to the provider.
func TestNativeMessagesWorkerCLIHelper(t *testing.T) {
	if os.Getenv("AMUX_MESSAGES_WORKER_CLI_HELPER") != "1" {
		return
	}
	project := os.Getenv("AMUX_MESSAGES_WORKER_PROJECT")
	body := os.Getenv("AMUX_MESSAGES_WORKER_BODY")
	if project == "" || body == "" {
		fmt.Fprintln(os.Stderr, "native worker message fixture is missing project or body")
		os.Exit(1)
	}
	if err := run(context.Background(), []string{"messages", "--project", project, "send", body}, os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runNativeMessagesWorkerCLI is called by the existing synthetic Codex
// app-server fixture when this test opts into its message-send branch.
func runNativeMessagesWorkerCLI() error {
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	identity := nativeMessagesWorkerIdentity{
		SessionID: os.Getenv("AMUX_AGENT_SESSION_ID"), InstanceID: os.Getenv("AMUX_AGENT_INSTANCE_ID"),
		Provider: os.Getenv("AMUX_AGENT_PROVIDER"), OS: os.Getenv("AMUX_AGENT_OS"),
		Directory: os.Getenv("AMUX_AGENT_PROJECT"), Project: os.Getenv("AMUX_GATEWAY_PROJECT"),
		GatewayURL: os.Getenv("AMUX_GATEWAY_URL"), APIKeyEnv: os.Getenv("AMUX_GATEWAY_API_KEY_ENV"),
	}
	if identity.Directory != directory || identity.Project != os.Getenv("AMUX_MESSAGES_WORKER_PROJECT") || identity.APIKeyEnv == "" || os.Getenv(identity.APIKeyEnv) == "" {
		return fmt.Errorf("provider environment does not contain the saved worker execution and API-key name")
	}
	identityBytes, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	if marker := os.Getenv("AMUX_MESSAGES_WORKER_MARKER"); marker == "" {
		return fmt.Errorf("native worker identity marker path is missing")
	} else if err := os.WriteFile(marker, identityBytes, 0600); err != nil {
		return err
	}
	command := exec.Command(os.Args[0], "-test.run=^TestNativeMessagesWorkerCLIHelper$")
	_, err = command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("native messages CLI failed: %w", err)
	}
	return nil
}

func TestNativeWorkerMessagesSendUsesSavedRunIdentityEnvironment(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX native-process fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "messages-remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "native-messages", ChannelName: "internal", RoomID: "native-messages", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "native-messages", Title: "Send an inline message", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const apiKeyEnv = "CUSTOM_GATEWAY_KEY"
	const apiKey = "synthetic-worker-gateway-secret"
	const body = "literal worker message\nwith a second line"
	var headersMu sync.Mutex
	var messageHeaders http.Header
	handler, err := server.NewHandler(remote, apiKey)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/projects/native-messages/messages" {
			headersMu.Lock()
			messageHeaders = r.Header.Clone()
			headersMu.Unlock()
		}
		handler.ServeHTTP(w, r)
	})
	host := httptest.NewServer(wrapped)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

	temp := t.TempDir()
	checkout := filepath.Join(temp, "saved-checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(checkout, "native-message-identity.json")
	fixtureError := filepath.Join(checkout, "native-message-cli-error.txt")
	command, err := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeCodexWorkerFixture$", "--"})
	if err != nil {
		t.Fatal(err)
	}
	process := startWorkerProcess(t, ctx, filepath.Join(temp, "worker.db"), map[string]string{
		"AMUX_CODEX_WORKER_FIXTURE":          "1",
		"AMUX_CODEX_WORKER_MESSAGES_FIXTURE": "1",
		"AMUX_MESSAGES_WORKER_CLI_HELPER":    "1",
		"AMUX_MESSAGES_WORKER_PROJECT":       "native-messages",
		"AMUX_MESSAGES_WORKER_BODY":          body,
		"AMUX_MESSAGES_WORKER_MARKER":        marker,
		"AMUX_MESSAGES_WORKER_ERROR":         fixtureError,
		apiKeyEnv:                            apiKey,
		"GATEWAY_API_KEY":                    "",
		"AMUX_AGENT_SESSION_ID":              "inherited-stale-session",
		"AMUX_AGENT_INSTANCE_ID":             "inherited-stale-instance",
		"AMUX_GATEWAY_PROJECT":               "inherited-stale-project",
		"AMUX_GATEWAY_URL":                   "https://inherited-stale.example",
	}, "--server", host.URL, "--project", "native-messages", "--task", task.ID, "--directory", checkout,
		"--provider", "codex", "--agent-command", string(command), "--api-key-env", apiKeyEnv,
		"--prompt", "Inspect the headless task", "--no-task-tools=false")

	var thread *storage.ProjectMessageThread
	for {
		listing, listErr := remote.ListProjectMessageThreads(ctx, "native-messages", storage.ProjectMessageThreadFilter{State: "all"})
		if listErr != nil {
			if ctx.Err() != nil {
				failure, _ := os.ReadFile(fixtureError)
				t.Fatalf("message wait expired: %v; fixture=%s; worker=%s", listErr, failure, process.stderr.String())
			}
			t.Fatal(listErr)
		}
		if len(listing.Threads) != 0 {
			thread, err = remote.GetProjectMessageThread(ctx, "native-messages", listing.Threads[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if _, err := os.Stat(fixtureError); err == nil {
			failure, _ := os.ReadFile(fixtureError)
			t.Fatalf("native provider could not send through the messages CLI: %s", failure)
		}
		select {
		case readErr := <-process.stdoutDone:
			failure, _ := os.ReadFile(fixtureError)
			t.Fatalf("worker exited before posting a message: %v; fixture=%s; stderr=%s", readErr, failure, process.stderr.String())
		case <-ctx.Done():
			t.Fatalf("worker did not send an inline message: %s", process.stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if thread.Root.Content != body || thread.Root.ProjectIdent != "native-messages" || thread.Root.Source != "agent" || thread.Root.AuthorKind == nil || *thread.Root.AuthorKind != "agent" || thread.Root.AgentID == nil {
		t.Fatalf("stored native message did not preserve body/sender: %+v", thread.Root)
	}
	stopWorkerProcess(t, ctx, process)

	local, err := storage.Open(ctx, filepath.Join(temp, "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	run, err := local.WorkerRun(ctx, process.runID)
	if err != nil {
		t.Fatal(err)
	}
	var actual nativeMessagesWorkerIdentity
	identityBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(identityBytes, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.SessionID != run.Owner.SessionID || actual.InstanceID != run.Owner.InstanceID || actual.Provider != run.Owner.Provider || actual.OS != run.Owner.OS || actual.Directory != run.Directory || actual.Project != run.Project || actual.GatewayURL != run.Server || actual.APIKeyEnv != apiKeyEnv {
		t.Fatalf("provider environment did not match saved run identity: got=%+v run=%+v", actual, run)
	}
	if *thread.Root.AgentID != run.Owner.SessionID {
		t.Fatalf("stored message sender=%q, saved worker session=%q", *thread.Root.AgentID, run.Owner.SessionID)
	}
	headersMu.Lock()
	gotHeaders := messageHeaders.Clone()
	headersMu.Unlock()
	for name, want := range map[string]string{
		"Authorization":       "Bearer " + apiKey,
		"X-Agent-Session-Id":  run.Owner.SessionID,
		"X-Agent-Instance-Id": run.Owner.InstanceID,
		"X-Agent-Provider":    run.Owner.Provider,
		"X-Agent-Os":          run.Owner.OS,
	} {
		if got := gotHeaders.Get(name); got != want {
			t.Errorf("message request header %s=%q, want saved run value", name, got)
		}
	}
}
