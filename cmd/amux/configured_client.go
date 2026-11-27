package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/clientconfig"
	"github.com/nitecon/amux/internal/clientcontrol"
	"github.com/nitecon/amux/internal/gatewayprocess"
	"github.com/nitecon/amux/internal/localconversation"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type configuredPoolKey struct {
	project, provider string
}

type configuredPoolRun struct {
	directory string
	drain     chan struct{}
	draining  bool
}

type configuredPoolExit struct {
	key configuredPoolKey
	err error
}

func runConfiguredClient(parent context.Context, path, state string, stdout, stderr io.Writer) error {
	owner, err := server.LockProfile(path)
	if err != nil {
		return err
	}
	defer owner.Close()
	config, err := clientconfig.Load(path)
	if err != nil {
		return fmt.Errorf("load client configuration: %w", err)
	}

	if state == "" {
		state = config.WorkerJournal
	}
	if state == "" {
		state = filepath.Join(filepath.Dir(path), "workers.db")
	}

	lock, err := server.LockProfile(state)
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	build, err := clientcontrol.ExecutableDigest(executable)
	if err != nil {
		return fmt.Errorf("identify running client binary: %w", err)
	}
	if hosted := config.HostedGateway; hosted != nil {
		startup, stopStartup := context.WithTimeout(ctx, 20*time.Second)
		_, _, stopGateway, err := gatewayprocess.Start(startup, executable, filepath.Dir(config.EnrolledProfile), hosted.InstanceID, hosted.AdvertisedHost, hosted.RemoteHosting, stderr)
		stopStartup()
		if err != nil {
			return fmt.Errorf("start configured local gateway: %w", err)
		}
		defer stopGateway()
	}
	client, _, err := gatewayClientForProfile(config.GatewayEndpoint, config.EnrolledProfile, os.Getenv(config.APIKeyEnv))
	if err != nil {
		return err
	}
	store, err := storage.Open(ctx, state)
	if err != nil {
		return err
	}
	defer store.Close()
	if config.HostedGateway != nil && client.Endpoint() == clientconfig.LocalGatewayEndpoint {
		if err := store.UpgradeLoopbackWorkerOrigins(ctx); err != nil {
			return fmt.Errorf("upgrade local gateway worker journal: %w", err)
		}
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	conversationRoot := filepath.Join(home, ".amux", "conversations")
	archive := localconversation.ArchiveTaskWorker(conversationRoot, client)
	if config.TaskToolsCommand != "" {
		executable, err = exec.LookPath(config.TaskToolsCommand)
		if err != nil {
			return err
		}
		executable, err = filepath.Abs(executable)
		if err != nil {
			return err
		}
	}
	open := worker.OpenWithNativeTools(worker.NativeToolsConfig{
		Executable: executable, Server: client.Endpoint(), EnrolledProfile: config.EnrolledProfile,
		APIKeyEnv: config.APIKeyEnv, WorkPreferencesPath: config.WorkPreferences,
	})
	active := make(map[configuredPoolKey]*configuredPoolRun)
	exits := make(chan configuredPoolExit)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		worker.WatchAssignments(ctx, store, client)
	}()
	defer func() {
		cancel()
		for len(active) > 0 {
			delete(active, (<-exits).key)
		}
		<-watchDone
	}()

	bindings, bindingErr := configuredBindings(ctx, client, config)
	if bindingErr != nil {
		fmt.Fprintf(stderr, "waiting for client project bindings: %v\n", bindingErr)
	}
	reloads := make(chan clientReload)
	signals, stopSignals := clientReloadSignals()
	defer stopSignals()
	var rustPresence *localconversation.RustPresence
	presenceDone := make(chan struct{})
	defer func() {
		if rustPresence != nil {
			cancel()
			<-presenceDone
		}
	}()
	taskApprovals := localconversation.NewTaskApprovals()
	observe := func(run storage.WorkerRun, id string, event conversation.Event) {
		info := storage.Conversation{ProjectIdent: run.Project, ID: run.ConversationID, Provider: run.Owner.Provider, Owner: run.Owner}
		taskApprovals.Observe(info, id, event, func(ctx context.Context, option string) error {
			return client.Enqueue(ctx, run.Project, run.ConversationID, uuid.NewString(), conversation.Command{
				Kind: "permission", PermissionEventID: id, OptionID: option,
			})
		})
	}
	legacyWorker := localconversation.LegacyTaskWorker{Store: store, Client: client, TranscriptRoot: conversationRoot, Open: open, Approvals: taskApprovals}
	capabilityKnown, native := false, false
	profile := clientconfig.Config{
		EnrolledProfile: config.EnrolledProfile, GatewayEndpoint: client.Endpoint(), APIKeyEnv: config.APIKeyEnv,
		HostedGateway: config.HostedGateway,
	}
	type workspaceSnapshot struct {
		clientcontrol.Workspace
		providers []string
	}
	executionCheckout := func(workspace clientcontrol.Workspace, project string) string {
		if workspace.ExcludedProjects[project] {
			return ""
		}
		return workspace.Projects[project]
	}
	var currentWorkspace atomic.Value
	fleetChanged := make(chan struct{}, 1)
	publishWorkspace := func() {
		currentWorkspace.Store(workspaceSnapshot{clientcontrol.Workspace{
			ProjectsDirectory: config.ProjectsDirectory, Projects: bindings, ExcludedProjects: config.ExcludedProjects,
		}, config.Providers})
		select {
		case fleetChanged <- struct{}{}:
		default:
		}
	}
	publishWorkspace()
	if config.EnrolledProfile != "" {
		fleetCtx, stopFleet := context.WithCancel(ctx)
		fleetDone := make(chan struct{})
		go func() {
			defer close(fleetDone)
			runFleetConversations(fleetCtx, store, client, open, localconversation.ArchiveFleetWorker(conversationRoot, client), observe, func(project, provider string) string {
				workspace := currentWorkspace.Load().(workspaceSnapshot)
				for _, enabled := range workspace.providers {
					if enabled == provider {
						return executionCheckout(workspace.Workspace, project)
					}
				}
				return ""
			}, fleetChanged, stderr)
		}()
		defer func() { stopFleet(); <-fleetDone }()
	}
	stopControl, err := serveClientControl(path, client.Endpoint(), build, reloads, ctx.Done(), func(mux *http.ServeMux) func() {
		mux.HandleFunc("GET /profile", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(profile)
		})
		mux.Handle("POST /checkout", configuredCheckoutHandler(client, reloads, ctx.Done(), func() string {
			return currentWorkspace.Load().(workspaceSnapshot).Workspace.ProjectsDirectory
		}))
		mux.HandleFunc("GET /workspace", func(w http.ResponseWriter, r *http.Request) {
			workspace := currentWorkspace.Load().(workspaceSnapshot).Workspace
			workspace.LocalCheckouts = projectidentity.DiscoverLocalCheckouts(r.Context(), workspace.ProjectsDirectory)
			_ = json.NewEncoder(w).Encode(workspace)
		})
		mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			cancel()
		})
		return localconversation.RegisterControl(mux, ctx, conversationRoot, func(project string) string {
			return executionCheckout(currentWorkspace.Load().(workspaceSnapshot).Workspace, project)
		}, worker.OpenWithNativeTools(worker.NativeToolsConfig{
			Executable: executable, Server: client.Endpoint(), EnrolledProfile: config.EnrolledProfile,
			APIKeyEnv: config.APIKeyEnv, WorkPreferencesPath: config.WorkPreferences, LocalConversation: true,
		}), taskApprovals)
	})
	if err != nil {
		return err
	}
	defer stopControl()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()

	type providerCheck struct {
		available bool
		at        time.Time
	}
	checks := make(map[string]providerCheck)
	reconcile := func() string {
		publishWorkspace()
		wanted := make(map[configuredPoolKey]string)
		report := make([]storage.CheckoutReportItem, 0, len(bindings))
		var failures []string
		for project, directory := range bindings {
			checkout, err := projectidentity.ResolveCheckout(ctx, directory)
			if err != nil {
				report = append(report, storage.CheckoutReportItem{Project: project, Unavailable: true})
				failures = append(failures, fmt.Sprintf("%s: %v", project, err))
				continue
			}
			item := storage.CheckoutReportItem{Project: project}
			if checkout.Upstream != "" {
				item.Origin = &checkout.Upstream
			}
			report = append(report, item)
			if config.ExcludedProjects[project] {
				continue
			}
			for _, provider := range config.Providers {
				wanted[configuredPoolKey{project, provider}] = checkout.Directory
			}
		}
		if !capabilityKnown && len(wanted) > 0 {
			capabilityCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			supported, err := client.SupportsNativeWorkerAssignment(capabilityCtx)
			stop()
			if err != nil {
				failures = append(failures, fmt.Sprintf("check gateway assignment capability: %v", err))
			} else {
				capabilityKnown, native = true, supported
				if !native {
					rustPresence = localconversation.NewRustPresence()
					go func() {
						defer close(presenceDone)
						rustPresence.Run(ctx, client)
					}()
				}
			}
		}
		var draining []string
		for key, run := range active {
			if run.directory != wanted[key] && !run.draining {
				run.draining = true
				close(run.drain)
			}
			if run.draining {
				draining = append(draining, key.project+"/"+key.provider)
			}
		}
		saved, err := store.WorkerPools(ctx, client.Endpoint())
		if err != nil {
			failures = append(failures, err.Error())
		} else {
			pools := make(map[configuredPoolKey]*storage.WorkerPool)
			probes := make(map[string]string)
			for key, directory := range wanted {
				if !capabilityKnown || active[key] != nil {
					continue
				}
				pool, err := configuredPool(ctx, store, client.Endpoint(), key, directory, saved)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s/%s: %v", key.project, key.provider, err))
					continue
				}
				pools[key] = pool
				if len(pool.Command) == 0 && time.Since(checks[key.provider].at) >= time.Minute {
					probes[key.provider] = directory
				}
			}
			type checkResult struct {
				provider  string
				available bool
			}
			probeCtx, stopProbes := context.WithTimeout(ctx, 4*time.Second)
			results := make(chan checkResult, len(probes))
			for provider, directory := range probes {
				go func() {
					results <- checkResult{provider, localconversation.ProviderAvailable(probeCtx, provider, directory)}
				}()
			}
			for range probes {
				result := <-results
				checks[result.provider] = providerCheck{available: result.available, at: time.Now()}
			}
			stopProbes()
			for key, pool := range pools {
				if len(pool.Command) == 0 && !checks[key.provider].available {
					failures = append(failures, fmt.Sprintf("%s/%s: provider unavailable", key.project, key.provider))
					continue
				}
				directory := wanted[key]
				run := &configuredPoolRun{directory: directory, drain: make(chan struct{})}
				active[key] = run
				go func() {
					var err error
					if native {
						err = worker.RunPool(ctx, worker.PoolConfig{Store: store, Client: client, PoolID: pool.ID, Open: open, Archive: archive, Observe: observe, Drain: run.drain, StreamAssignments: true})
					} else {
						err = legacyWorker.Run(ctx, *pool, run.drain, rustPresence)
					}
					exits <- configuredPoolExit{key, err}
				}()
			}
		}
		reportCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if _, err := client.SyncCheckouts(reportCtx, hostname, report); err != nil {
			failures = append(failures, err.Error())
		}
		sort.Strings(draining)
		sort.Strings(failures)
		return fmt.Sprintf("Applied %d project bindings; draining [%s]; failures [%s].", len(bindings), strings.Join(draining, ", "), strings.Join(failures, "; "))
	}

	reload := func(change *clientcontrol.CheckoutChange) string {
		candidate, err := clientconfig.Load(path)
		if err == nil && (candidate.EnrolledProfile != config.EnrolledProfile || candidate.GatewayEndpoint != config.GatewayEndpoint ||
			candidate.APIKeyEnv != config.APIKeyEnv || candidate.TaskToolsCommand != config.TaskToolsCommand || candidate.WorkPreferences != config.WorkPreferences) {
			err = fmt.Errorf("changing gateway credentials or task tool settings requires restarting the client")
		}
		if err == nil && !clientconfig.SameHostedGateway(config.HostedGateway, candidate.HostedGateway) {
			err = fmt.Errorf("changing hosted_gateway requires restarting the client")
		}
		if err == nil && change != nil {
			if change.ExcludeExecution != nil {
				if *change.ExcludeExecution {
					if candidate.ExcludedProjects == nil {
						candidate.ExcludedProjects = make(map[string]bool)
					}
					candidate.ExcludedProjects[change.Project] = true
				} else {
					delete(candidate.ExcludedProjects, change.Project)
				}
			} else if change.ProjectsDirectory != nil {
				candidate.ProjectsDirectory = *change.ProjectsDirectory
			} else if change.Checkout == "" {
				delete(candidate.Projects, change.Project)
			} else {
				if candidate.Projects == nil {
					candidate.Projects = make(map[string]string)
				}
				candidate.Projects[change.Project] = change.Checkout
			}
			err = candidate.Validate()
		}
		if err == nil && candidate.WorkerJournal != config.WorkerJournal {
			err = fmt.Errorf("changing worker_journal requires restarting the client")
		}
		var candidateBindings map[string]string
		if err == nil {
			recreate := ""
			if change != nil && change.Recreate {
				recreate = change.Project
			}
			candidateBindings, err = resolveConfiguredBindings(ctx, client, candidate, recreate)
		}
		if err != nil {
			return "Rejected configuration: " + err.Error()
		}
		if change != nil {
			if err := clientconfig.Save(path, candidate); err != nil {
				return "Rejected configuration: " + err.Error()
			}
		}
		config, bindings, bindingErr = candidate, candidateBindings, nil
		return reconcile()
	}
	if bindingErr == nil {
		fmt.Fprintln(stdout, reconcile())
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-signals:
			fmt.Fprintln(stdout, reload(nil))
		case request := <-reloads:
			result := reload(request.change)
			fmt.Fprintln(stdout, result)
			request.reply <- result
		case <-tick.C:
			if updated, err := configuredBindings(ctx, client, config); err == nil {
				bindings, bindingErr = updated, nil
			}
			if bindingErr == nil {
				reconcile()
			}
		case exit := <-exits:
			run := active[exit.key]
			delete(active, exit.key)
			if exit.err != nil {
				fmt.Fprintf(stderr, "project %s/%s: worker stopped: %v\n", exit.key.project, exit.key.provider, exit.err)
			}
			if run.draining {
				reconcile()
			}
		}
	}
}

func configuredBindings(parent context.Context, client *gatewayclient.Client, config clientconfig.Config) (map[string]string, error) {
	return resolveConfiguredBindings(parent, client, config, "")
}

func resolveConfiguredBindings(parent context.Context, client *gatewayclient.Client, config clientconfig.Config, recreate string) (map[string]string, error) {
	bindings := make(map[string]string, len(config.Projects))
	if config.ProjectsDirectory != "" {
		info, err := os.Stat(config.ProjectsDirectory)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("projects_directory is not a directory")
		}
	}
	if config.ProjectsDirectory == "" && len(config.Projects) == 0 && len(config.ProjectURLs) == 0 {
		return bindings, nil
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	inventory, err := client.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read gateway projects: %w", err)
	}
	checkouts := make(map[string]string)
	for _, checkout := range projectidentity.DiscoverLocalCheckouts(ctx, config.ProjectsDirectory) {
		if checkout.CanonicalRemote != nil && checkouts[*checkout.CanonicalRemote] == "" {
			checkouts[*checkout.CanonicalRemote] = checkout.Directory
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	byID := make(map[string]storage.ProjectOverview, len(inventory))
	byRemote := make(map[string][]string)
	for _, project := range inventory {
		byID[project.Ident] = project
		if project.ArchivedAt != nil || project.CanonicalRemote == nil {
			continue
		}
		remote, err := projects.CanonicalRemote(*project.CanonicalRemote)
		if err == nil {
			byRemote[remote] = append(byRemote[remote], project.Ident)
			if directory := checkouts[remote]; directory != "" {
				bindings[project.Ident] = directory
			}
		}
	}
	pending := make(map[string]string)
	for id, path := range config.Projects {
		checkout, err := projectidentity.ResolveCheckout(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", id, err)
		}
		if project, exists := byID[id]; exists {
			if project.ArchivedAt != nil {
				return nil, fmt.Errorf("project %q is archived on the gateway", id)
			}
			if err := projectidentity.ValidateBinding(project.Kind, project.CanonicalRemote, checkout); err != nil {
				return nil, fmt.Errorf("project %s: %w", id, err)
			}
		} else {
			if _, err := projects.ParseRemote(checkout.Upstream); err != nil || projects.SanitizeIdent(id) != id {
				return nil, fmt.Errorf("new project %q requires a valid project ID and Git origin", id)
			}
			pending[id] = checkout.Upstream
			byRemote[checkout.Upstream] = append(byRemote[checkout.Upstream], id)
		}
		bindings[id] = checkout.Directory
	}
	for _, url := range config.ProjectURLs {
		repo, err := projects.ParseRemote(url)
		if err != nil {
			return nil, err
		}
		ids := byRemote[repo.Canonical]
		switch len(ids) {
		case 0:
			id := projects.SanitizeIdent(repo.Name)
			if _, exists := byID[id]; exists || pending[id] != "" {
				return nil, fmt.Errorf("project ID %q is already used by a different or archived project", id)
			}
			pending[id] = repo.Canonical
			byRemote[repo.Canonical] = []string{id}
			if directory := checkouts[repo.Canonical]; directory != "" {
				bindings[id] = directory
			}
		case 1:
			// The inventory match or explicit binding already selected the checkout.
		default:
			return nil, fmt.Errorf("configured Git remote %q matches multiple active gateway projects", repo.Canonical)
		}
	}
	if len(pending) == 0 {
		return bindings, nil
	}
	for id, remote := range pending {
		if err := client.RegisterRepository(ctx, id, remote, id == recreate); err != nil {
			var response *gatewayclient.HTTPError
			if id != recreate && errors.As(err, &response) && response.Status == http.StatusGone {
				delete(bindings, id)
				delete(pending, id)
				continue
			}
			return nil, fmt.Errorf("register gateway project %s: %w", id, err)
		}
	}
	if len(pending) == 0 {
		return bindings, nil
	}
	inventory, err = client.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify registered gateway projects: %w", err)
	}
	for _, project := range inventory {
		expected, exists := pending[project.Ident]
		if !exists || project.ArchivedAt != nil || project.CanonicalRemote == nil {
			continue
		}
		remote, err := projects.CanonicalRemote(*project.CanonicalRemote)
		if err == nil && remote == expected {
			delete(pending, project.Ident)
		}
	}
	if len(pending) != 0 {
		return nil, fmt.Errorf("gateway did not confirm the configured project registrations")
	}
	return bindings, nil
}

func configuredPool(ctx context.Context, store *storage.Store, endpoint string, key configuredPoolKey, directory string, saved []storage.WorkerPool) (*storage.WorkerPool, error) {
	for _, pool := range saved {
		if pool.Project == key.project && pool.Provider == key.provider && pool.Directory == directory {
			return &pool, nil
		}
	}
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join([]string{"amux-config-worker", endpoint, key.project, key.provider, directory}, "\x00")))
	return store.CreateWorkerPool(ctx, storage.WorkerPool{ID: id.String(), Server: endpoint, Project: key.project, Directory: directory, Provider: key.provider, OS: platform, SlotCount: 1})
}
