package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func runWorker(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := flag.NewFlagSet("amux worker run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	state := flags.String("state", "", "local durable worker SQLite path (required)")
	endpoint := flags.String("server", "", "gateway HTTP(S) origin (required for a new run)")
	enrolledProfile := flags.String("enrolled-profile", "", "enrolled gateway credential manifest")
	runID := flags.String("run-id", "", "resume a saved worker run with its exact native session")
	project := flags.String("project", "", "server project identifier for a new run")
	task := flags.String("task", "", "server task UUID for a new run")
	directory := flags.String("directory", "", "existing checkout directory for a new run")
	provider := flags.String("provider", "antigravity", "native provider (antigravity, codex or claude)")
	command := flags.String("agent-command", "", "optional native stdio command as a JSON string array; no shell")
	prompt := flags.String("prompt", "", "initial prompt for a new run; further commands arrive from the gateway")
	goalFlag := flags.String("goal", "", "select the conversation task goal for this run")
	keyEnv := flags.String("api-key-env", "GATEWAY_API_KEY", "environment variable containing the gateway API credential")
	taskTools := flags.String("task-tools-command", "", "optional Amux MCP executable override")
	noTaskTools := flags.Bool("no-task-tools", false, "disable native amux task tools for this launch")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected worker arguments")
	}
	if *enrolledProfile != "" {
		var err error
		*enrolledProfile, err = absoluteEnrolledManifest(*enrolledProfile)
		if err != nil {
			return err
		}
	}
	providerSet := false
	goalWasSet := false
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "provider":
			providerSet = true
		case "goal":
			goalWasSet = true
		}
	})
	if *state == "" {
		return fmt.Errorf("--state is required")
	}
	if *runID != "" && (*project != "" || *task != "" || *directory != "" || *command != "" || *prompt != "" || goalWasSet) {
		return fmt.Errorf("--run-id resumes saved configuration; creation flags cannot replace it")
	}
	if *runID == "" && ((*endpoint == "" && *enrolledProfile == "") || *project == "" || *task == "" || *directory == "") {
		return fmt.Errorf("new workers require --server or --enrolled-profile, --project, --task and --directory")
	}
	var selectedGoal *string
	if goalWasSet {
		var err error
		selectedGoal, err = storage.NormalizeTaskGoal(goalFlag)
		if err != nil {
			return err
		}
	}
	var nativeCommand []string
	if *command != "" {
		if err := json.Unmarshal([]byte(*command), &nativeCommand); err != nil || len(nativeCommand) == 0 || len(nativeCommand) > 64 || len(*command) > 65536 || nativeCommand[0] == "" {
			return fmt.Errorf("--agent-command must be a nonempty JSON string array")
		}
	}
	lock, err := server.LockProfile(*state)
	if err != nil {
		return err
	}
	defer lock.Close()
	local, err := storage.Open(ctx, *state)
	if err != nil {
		return err
	}
	defer local.Close()
	var saved *storage.WorkerRun
	if *runID != "" {
		saved, err = local.WorkerRun(ctx, *runID)
		if err != nil {
			return err
		}
		if *endpoint != "" && *endpoint != saved.Server {
			return fmt.Errorf("saved worker belongs to another gateway")
		}
		if providerSet && *provider != saved.Owner.Provider {
			return fmt.Errorf("saved worker uses provider %q; --provider cannot replace it", saved.Owner.Provider)
		}
		*endpoint = saved.Server
	}
	client, resolvedEndpoint, err := gatewayClientForProfile(*endpoint, *enrolledProfile, getenv(*keyEnv))
	if err != nil {
		return err
	}
	*endpoint = resolvedEndpoint
	if saved == nil {
		// The saved run is written before an initial prompt is queued. If a queue
		// response is lost, resuming the printed run ID retains the original intent.
		saved, _, err = worker.PrepareRun(ctx, local, client, *project, *task, *directory, *provider, nativeCommand, selectedGoal)
		if err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintln(stdout, "worker run", saved.ID, "conversation", saved.ConversationID); err != nil {
		return err
	}
	if *prompt != "" {
		if err = client.Enqueue(ctx, saved.Project, saved.ConversationID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: *prompt}); err != nil {
			return err
		}
	}
	var open worker.Open
	if !*noTaskTools {
		executable := *taskTools
		if executable == "" {
			executable, err = os.Executable()
		} else {
			executable, err = exec.LookPath(executable)
			if err == nil {
				executable, err = filepath.Abs(executable)
			}
		}
		if err != nil {
			return err
		}
		open = worker.OpenWithNativeTools(worker.NativeToolsConfig{Executable: executable, Server: client.Endpoint(), APIKeyEnv: *keyEnv, EnrolledProfile: *enrolledProfile})
	}
	err = worker.Run(ctx, worker.Config{Store: local, Client: client, RunID: saved.ID, Open: open, AdvertisePresence: true})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}
