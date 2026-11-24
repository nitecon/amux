package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type automaticWorkerScope struct {
	project   storage.ProjectOverview
	directory string
}

func (b *Browser) updateAutomaticWorkerScopes() {
	if b.automaticWorkerScopes == nil {
		return
	}
	var scopes []automaticWorkerScope
	for _, project := range b.projectItems {
		directory := b.projectCheckout(project).Directory
		if project.ArchivedAt == nil && directory != "" {
			scopes = append(scopes, automaticWorkerScope{project, directory})
		}
	}
	select {
	case <-b.automaticWorkerScopes:
	default:
	}
	b.automaticWorkerScopes <- scopes
}

type automaticWorkerRun struct {
	drain    chan struct{}
	done     chan error
	draining bool
}

func (m *WorkerManager) runAutomaticWorkers(ctx context.Context, updates <-chan []automaticWorkerScope, report func(string), probe func(context.Context, string, string) bool) {
	active := make(map[string]*automaticWorkerRun)
	providers := make(map[string]bool)
	var scopes []automaticWorkerScope
	var workers sync.WaitGroup
	var presence *rustPresence
	defer workers.Wait()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var checked time.Time
	capabilitiesKnown, native := false, false
	for {
		select {
		case <-ctx.Done():
			return
		case scopes = <-updates:
		case <-ticker.C:
		}
		if !capabilitiesKnown {
			check, cancel := context.WithTimeout(ctx, 5*time.Second)
			available, err := m.client.SupportsNativeWorkerAssignment(check)
			cancel()
			if err != nil {
				report("Automatic agents: could not check gateway support.")
				continue
			}
			if !available && (!m.useTaskStream || m.taskConversationRoot == "") {
				report("Automatic agents: the gateway does not support native worker assignment.")
				continue
			}
			capabilitiesKnown, native = true, available
			if !native {
				presence = &rustPresence{agents: make(map[string]gatewayclient.RustPresenceAgent), changed: make(chan struct{}, 1)}
				workers.Add(1)
				go func() {
					defer workers.Done()
					presence.run(ctx, m.client)
				}()
			}
		}
		if len(scopes) > 0 && time.Since(checked) >= time.Minute {
			for _, provider := range defaultChatProviders {
				providers[provider] = probe(ctx, provider, scopes[0].directory)
			}
			checked = time.Now()
		}
		var lastError error
		for id, run := range active {
			select {
			case err := <-run.done:
				if err != nil {
					lastError = err
				}
				delete(active, id)
			default:
			}
		}
		wanted := make(map[string]bool)
		for _, scope := range scopes {
			identity, err := projectidentity.ResolveCheckout(ctx, scope.directory)
			if err != nil || validateWorkspaceCheckoutProject(scope.project, identity) != nil {
				continue
			}
			for _, provider := range defaultChatProviders {
				if !providers[provider] {
					continue
				}
				id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join([]string{"amux-auto-worker", m.client.Endpoint(), scope.project.Ident, provider}, "\x00"))).String()
				wanted[id] = true
				existing := active[id]
				if existing != nil {
					continue
				}
				pool, err := m.automaticPool(ctx, id, scope.project.Ident, identity.Directory, provider)
				if err != nil {
					lastError = err
					continue
				}
				run := &automaticWorkerRun{drain: make(chan struct{}), done: make(chan error, 1)}
				active[id] = run
				workers.Add(1)
				go func() {
					defer workers.Done()
					if !native {
						run.done <- m.runLegacyTaskWorker(ctx, *pool, run.drain, presence)
						return
					}
					run.done <- worker.RunPool(ctx, worker.PoolConfig{
						Store: m.store, Client: m.client, PoolID: pool.ID, Open: m.open, Archive: m.archive, Drain: run.drain, StreamAssignments: m.useTaskStream,
					})
				}()
			}
		}
		for id, run := range active {
			if !wanted[id] && !run.draining {
				run.draining = true
				close(run.drain)
			}
		}
		if lastError != nil {
			report("Automatic agents: " + lastError.Error())
		} else {
			report(fmt.Sprintf("Automatic agents: %d local workers started.", len(active)))
		}
	}
}

func (m *WorkerManager) automaticPool(ctx context.Context, id, project, directory, provider string) (*storage.WorkerPool, error) {
	pool, err := m.store.WorkerPool(ctx, id)
	if errors.Is(err, storage.ErrWorkerPoolNotFound) {
		machineOS := runtime.GOOS
		if machineOS == "darwin" {
			machineOS = "macos"
		}
		command, commandErr := providerLaunchCommand(provider, ConversationLaunchPreferences{})
		if commandErr != nil {
			return nil, commandErr
		}
		if len(command) == 0 {
			command = []string{"agy", "acp", "--stdio"}
		}
		command[0] = localProviderExecutable(command[0])
		return m.store.CreateWorkerPool(ctx, storage.WorkerPool{
			ID: id, Server: m.client.Endpoint(), Project: project, Directory: directory, Provider: provider, OS: machineOS, SlotCount: 1, Command: command,
		})
	}
	if err == nil && pool.Directory != directory {
		return nil, fmt.Errorf("saved automatic checkout for %s has moved", project)
	}
	return pool, err
}

func providerAvailable(parent context.Context, provider, directory string) bool {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	switch provider {
	case "claude":
		data, err := exec.CommandContext(ctx, localProviderExecutable("claude"), "auth", "status", "--json").Output()
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		return err == nil && json.Unmarshal(data, &status) == nil && status.LoggedIn
	case "codex":
		return exec.CommandContext(ctx, localProviderExecutable("codex"), "login", "status").Run() == nil
	case "antigravity":
		session, err := worker.OpenNative(ctx, storage.WorkerRun{
			Directory: directory, Owner: storage.AgentOrigin{Provider: provider},
			Command: []string{localProviderExecutable("agy"), "acp", "--stdio"},
		}, func(context.Context, conversation.Event) error { return nil })
		if err != nil {
			return false
		}
		_ = session.Close()
		return true
	default:
		return false
	}
}
