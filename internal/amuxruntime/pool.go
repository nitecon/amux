package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/providerprocess"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
	"github.com/urfave/cli/v3"
)

func runPool(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	return runPoolMode(ctx, args, stdout, stderr, getenv, false)
}

func runPoolMode(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, registerOnly bool) error {
	flags := commandFlags("amux worker pool", stderr)
	state := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "state", Value: "", Usage: "durable local worker SQLite path (required)", Destination: state})
	poolID := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "pool-id", Value: "", Usage: "resume a saved pool and its active native sessions", Destination: poolID})
	endpoint := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "server", Value: "", Usage: "gateway HTTP(S) origin for a new pool", Destination: endpoint})
	enrolledProfile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "enrolled gateway credential manifest", Destination: enrolledProfile})
	project := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "project", Value: "", Usage: "project identifier for a new pool", Destination: project})
	directory := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "directory", Value: "", Usage: "existing checkout directory for a new pool", Destination: directory})
	provider := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "provider", Value: "antigravity", Usage: "native provider: antigravity, codex or claude", Destination: provider})
	slots := new(int)
	flags.Flags = append(flags.Flags, &cli.IntFlag{Name: "slots", Value: 1, Usage: "number of concurrent native workers (1-32)", Destination: slots})
	command := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "agent-command", Value: "", Usage: "native stdio command as a JSON string array; no shell", Destination: command})
	goalFlag := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "goal", Value: "", Usage: "select a task goal for this pool", Destination: goalFlag})
	keyEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "environment variable containing the gateway credential", Destination: keyEnv})
	noTaskTools := new(bool)
	flags.Flags = append(flags.Flags, &cli.BoolFlag{Name: "no-task-tools", Value: false, Usage: "disable native amux task tools for this launch", Destination: noTaskTools})
	if err := parseCommand(ctx, flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("unexpected worker pool arguments")
	}
	if *enrolledProfile != "" {
		var err error
		*enrolledProfile, err = absoluteEnrolledManifest(*enrolledProfile)
		if err != nil {
			return err
		}
	}
	goalWasSet := flags.IsSet("goal")
	if *state == "" {
		return fmt.Errorf("--state is required")
	}
	if registerOnly && *poolID != "" {
		return fmt.Errorf("-add-project registers an existing checkout, not a saved pool ID")
	}
	if *poolID != "" {
		for _, name := range []string{"project", "directory", "provider", "slots", "agent-command", "goal"} {
			if flags.IsSet(name) {
				return fmt.Errorf("--pool-id resumes saved configuration; --%s cannot replace it", name)
			}
		}
	} else if (*endpoint == "" && *enrolledProfile == "") || (!registerOnly && *project == "") || *directory == "" {
		return fmt.Errorf("new pools require --server or --enrolled-profile, --project and --directory")
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
	if registerOnly {
		checkout, err := projectidentity.ResolveCheckout(ctx, *directory)
		if err != nil {
			return fmt.Errorf("add project at %q: %w", *directory, err)
		}
		client, _, err := gatewayClientForProfile(*endpoint, *enrolledProfile, getenv(*keyEnv))
		if err != nil {
			return err
		}
		inventory, err := client.Projects(ctx)
		if err != nil {
			return fmt.Errorf("read gateway projects: %w", err)
		}
		var matches []string
		for _, candidate := range inventory {
			if *project != "" && candidate.Ident != projects.NormalizeIdent(*project) {
				continue
			}
			if candidate.CanonicalRemote == nil {
				if *project != "" && checkout.Upstream == "" {
					matches = append(matches, candidate.Ident)
				}
				continue
			}
			canonical, err := projects.CanonicalRemote(*candidate.CanonicalRemote)
			if err == nil && canonical == checkout.Upstream {
				matches = append(matches, candidate.Ident)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("checkout %q must match exactly one existing gateway project; specify --project to disambiguate", checkout.Directory)
		}
		*project, *directory = matches[0], checkout.Directory
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
	var pool *storage.WorkerPool
	if *poolID != "" {
		pool, err = local.WorkerPool(ctx, *poolID)
		if err != nil {
			return err
		}
		if *endpoint != "" && *endpoint != pool.Server {
			return fmt.Errorf("saved pool belongs to another gateway")
		}
		*endpoint = pool.Server
	}
	client, resolvedEndpoint, err := gatewayClientForProfile(*endpoint, *enrolledProfile, getenv(*keyEnv))
	if err != nil {
		return err
	}
	*endpoint = resolvedEndpoint
	if registerOnly {
		pools, err := local.WorkerPools(ctx, client.Endpoint())
		if err != nil {
			return err
		}
		for _, saved := range pools {
			if saved.Project != *project || saved.Directory != *directory || saved.Provider != *provider {
				continue
			}
			if saved.SlotCount != *slots || !slices.Equal(saved.Command, nativeCommand) ||
				(saved.Goal == nil) != (selectedGoal == nil) || saved.Goal != nil && *saved.Goal != *selectedGoal {
				return fmt.Errorf("checkout already has a pool for %s with different settings", *provider)
			}
			_, err := fmt.Fprintln(stdout, "worker pool", saved.ID)
			return err
		}
	}
	if pool == nil {
		absolute, err := providerprocess.ValidateDirectory(*directory, *provider)
		if err != nil {
			return err
		}
		platform := runtime.GOOS
		if platform == "darwin" {
			platform = "macos"
		}
		pool, err = local.CreateWorkerPool(ctx, storage.WorkerPool{ID: uuid.NewString(), Server: client.Endpoint(), Project: *project, Directory: absolute, Provider: *provider, OS: platform, Command: nativeCommand, SlotCount: *slots, Goal: selectedGoal})
		if err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(stdout, "worker pool", pool.ID); err != nil {
		return err
	}
	var open worker.Open
	if registerOnly {
		return nil
	}
	if !*noTaskTools {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		open = worker.OpenWithNativeTools(worker.NativeToolsConfig{Executable: executable, Server: client.Endpoint(), APIKeyEnv: *keyEnv, EnrolledProfile: *enrolledProfile})
	}
	err = worker.RunPool(ctx, worker.PoolConfig{Store: local, Client: client, PoolID: pool.ID, Open: open})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}
