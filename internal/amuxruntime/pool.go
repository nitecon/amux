package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
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
)

func runPool(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	return runPoolMode(ctx, args, stdout, stderr, getenv, false)
}

func runPoolMode(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, registerOnly bool) error {
	flags := flag.NewFlagSet("amux worker pool", flag.ContinueOnError)
	flags.SetOutput(stderr)
	state := flags.String("state", "", "durable local worker SQLite path (required)")
	poolID := flags.String("pool-id", "", "resume a saved pool and its active native sessions")
	endpoint := flags.String("server", "", "gateway HTTP(S) origin for a new pool")
	enrolledProfile := flags.String("enrolled-profile", "", "enrolled gateway credential manifest")
	project := flags.String("project", "", "project identifier for a new pool")
	directory := flags.String("directory", "", "existing checkout directory for a new pool")
	provider := flags.String("provider", "antigravity", "native provider: antigravity, codex or claude")
	slots := flags.Int("slots", 1, "number of concurrent native workers (1-32)")
	command := flags.String("agent-command", "", "native stdio command as a JSON string array; no shell")
	goalFlag := flags.String("goal", "", "select a task goal for this pool")
	keyEnv := flags.String("api-key-env", "GATEWAY_API_KEY", "environment variable containing the gateway credential")
	noTaskTools := flags.Bool("no-task-tools", false, "disable native amux task tools for this launch")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected worker pool arguments")
	}
	if *enrolledProfile != "" {
		var err error
		*enrolledProfile, err = absoluteEnrolledManifest(*enrolledProfile)
		if err != nil {
			return err
		}
	}
	goalWasSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "goal" {
			goalWasSet = true
		}
	})
	if *state == "" {
		return fmt.Errorf("--state is required")
	}
	if registerOnly && *poolID != "" {
		return fmt.Errorf("-add-project registers an existing checkout, not a saved pool ID")
	}
	if *poolID != "" {
		var conflict string
		flags.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "project", "directory", "provider", "slots", "agent-command", "goal":
				conflict = f.Name
			}
		})
		if conflict != "" {
			return fmt.Errorf("--pool-id resumes saved configuration; --%s cannot replace it", conflict)
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
