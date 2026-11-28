package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
	"github.com/urfave/cli/v3"
)

func runWorker(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := commandFlags("amux worker run", stderr)
	state := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "state", Value: "", Usage: "local durable worker SQLite path (required)", Destination: state})
	endpoint := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "server", Value: "", Usage: "gateway HTTP(S) origin (required for a new run)", Destination: endpoint})
	enrolledProfile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "enrolled gateway credential manifest", Destination: enrolledProfile})
	runID := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "run-id", Value: "", Usage: "resume a saved worker run with its exact native session", Destination: runID})
	project := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "project", Value: "", Usage: "server project identifier for a new run", Destination: project})
	task := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "task", Value: "", Usage: "server task UUID for a new run", Destination: task})
	directory := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "directory", Value: "", Usage: "existing checkout directory for a new run", Destination: directory})
	provider := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "provider", Value: "antigravity", Usage: "native provider (antigravity, codex or claude)", Destination: provider})
	command := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "agent-command", Value: "", Usage: "optional native stdio command as a JSON string array; no shell", Destination: command})
	prompt := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "prompt", Value: "", Usage: "initial prompt for a new run; further commands arrive from the gateway", Destination: prompt})
	goalFlag := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "goal", Value: "", Usage: "select the conversation task goal for this run", Destination: goalFlag})
	keyEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "environment variable containing the gateway API credential", Destination: keyEnv})
	taskTools := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "task-tools-command", Value: "", Usage: "optional Amux MCP executable override", Destination: taskTools})
	noTaskTools := new(bool)
	flags.Flags = append(flags.Flags, &cli.BoolFlag{Name: "no-task-tools", Value: false, Usage: "disable native amux task tools for this launch", Destination: noTaskTools})
	if err := parseCommand(ctx, flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("unexpected worker arguments")
	}
	if *enrolledProfile != "" {
		var err error
		*enrolledProfile, err = absoluteEnrolledManifest(*enrolledProfile)
		if err != nil {
			return err
		}
	}
	providerSet := flags.IsSet("provider")
	goalWasSet := flags.IsSet("goal")

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
	client, resolvedEndpoint, err := gatewayclient.NewForProfile(*endpoint, *enrolledProfile, getenv(*keyEnv))
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

func RunSavedWorker(ctx context.Context, store *storage.Store, client *gatewayclient.Client, runID, preferences, keyEnv, profile string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	open := worker.OpenWithNativeTools(worker.NativeToolsConfig{Executable: executable, Server: client.Endpoint(),
		EnrolledProfile: profile, APIKeyEnv: keyEnv, WorkPreferencesPath: preferences})
	return worker.Run(ctx, worker.Config{Store: store, Client: client, RunID: runID, Open: open, AdvertisePresence: true})
}
