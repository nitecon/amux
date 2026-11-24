package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/providerprocess"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type WorkerManagerConfig struct {
	Store                *storage.Store
	Client               *gatewayclient.Client
	Open                 worker.Open
	ConversationRoot     string
	TaskConversationRoot string
	UseTaskStream        bool
}

type WorkerManagerState string

const (
	WorkerManagerIdle     WorkerManagerState = "idle"
	WorkerManagerStarting WorkerManagerState = "starting"
	WorkerManagerRunning  WorkerManagerState = "running"
	WorkerManagerStopping WorkerManagerState = "stopping"
	WorkerManagerStopped  WorkerManagerState = "stopped"
	WorkerManagerFailed   WorkerManagerState = "failed"
)

type WorkerManagerSnapshot struct {
	State      WorkerManagerState
	Pool       *storage.WorkerPool
	Generation uint64
	Error      string
}

type workerManagerRun struct {
	generation uint64
	done       chan struct{}
	err        error
}

// WorkerManager owns one RunPool lifetime. The caller owns the profile lock and
// Store lifetime and must wait for Close before closing the Store.
type WorkerManager struct {
	mu                   sync.Mutex
	store                *storage.Store
	client               *gatewayclient.Client
	open                 worker.Open
	archive              worker.EventArchive
	useTaskStream        bool
	taskStreamDone       chan struct{}
	conversationRoot     string
	taskConversationRoot string

	ctx        context.Context
	cancel     context.CancelFunc
	poolStop   context.CancelFunc
	current    *workerManagerRun
	generation uint64
	active     bool
	started    bool
	closed     bool
	snapshot   WorkerManagerSnapshot
}

func NewWorkerManager(config WorkerManagerConfig) (*WorkerManager, error) {
	if config.Store == nil || config.Client == nil {
		return nil, fmt.Errorf("worker manager requires a local store and gateway client")
	}
	var archive worker.EventArchive
	if config.TaskConversationRoot != "" {
		archive = archiveTaskWorker(config.TaskConversationRoot, config.Client)
	}
	return &WorkerManager{
		store: config.Store, client: config.Client, open: config.Open, archive: archive, useTaskStream: config.UseTaskStream, conversationRoot: config.ConversationRoot, taskConversationRoot: config.TaskConversationRoot,
		snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle},
	}, nil
}

// Start binds the manager to its owning view lifetime. A manager can be
// started only once; subsequent pool runs share this lifetime context.
func (m *WorkerManager) Start(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("worker manager requires a lifetime context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("worker manager is closed")
	}
	if m.started {
		return fmt.Errorf("worker manager is already started")
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.started = true
	if m.useTaskStream {
		m.taskStreamDone = make(chan struct{})
		go func() {
			defer close(m.taskStreamDone)
			worker.WatchAssignments(m.ctx, m.store, m.client)
		}()
	}
	return nil
}

// RunConversation creates a taskless conversation and runs its native worker
// for the caller's lifetime. onCreated may open the transcript immediately.
func (m *WorkerManager) RunConversation(ctx context.Context, project, directory, provider string, onCreated func(storage.Conversation)) error {
	return m.RunConversationWithCommand(ctx, project, directory, provider, nil, onCreated)
}

// RunConversationWithCommand persists the provider's launch arguments with the
// worker run so exact session recovery uses the same preferences.
func (m *WorkerManager) RunConversationWithCommand(ctx context.Context, project, directory, provider string, command []string, onCreated func(storage.Conversation)) error {
	directory, err := m.conversationDirectory(ctx, project, directory)
	if err != nil {
		return err
	}
	if directory == "" {
		if m.conversationRoot == "" || project == "" || projects.SanitizeIdent(project) != project {
			return fmt.Errorf("a valid project and conversation directory are required")
		}
		directory = filepath.Join(m.conversationRoot, project, "conversations")
		if err := os.MkdirAll(directory, 0700); err != nil {
			return fmt.Errorf("create project conversation directory: %w", err)
		}
	}
	run, chat, err := worker.PrepareRun(ctx, m.store, m.client, project, "", directory, provider, command, nil)
	if err != nil {
		return err
	}
	onCreated(*chat)
	return worker.Run(ctx, worker.Config{Store: m.store, Client: m.client, RunID: run.ID, Open: m.open, AdvertisePresence: true})
}

func (m *WorkerManager) ResumeConversation(ctx context.Context, chat storage.Conversation) error {
	id, err := m.store.ConversationWorkerRunID(ctx, m.client.Endpoint(), chat.ProjectIdent, chat.ID)
	if err != nil || id == "" {
		return err
	}
	run, err := m.store.WorkerRun(ctx, id)
	if err != nil {
		return err
	}
	directory, err := m.conversationDirectory(ctx, chat.ProjectIdent, run.Directory)
	if err != nil {
		return err
	}
	if directory != run.Directory {
		return fmt.Errorf("saved conversation working directory is not the checkout root; start a new conversation from the attached checkout")
	}
	return worker.Run(ctx, worker.Config{Store: m.store, Client: m.client, RunID: id, Open: m.open, AdvertisePresence: true})
}

func (m *WorkerManager) conversationDirectory(ctx context.Context, project, directory string) (string, error) {
	items, err := m.client.Projects(ctx)
	if err != nil {
		return "", fmt.Errorf("verify conversation project: %w", err)
	}
	for _, item := range items {
		if item.Ident != project {
			continue
		}
		if item.Kind == "adhoc" && item.CanonicalRemote == nil {
			return directory, nil
		}
		if directory == "" {
			return "", fmt.Errorf("attach the source checkout to this workspace before starting a conversation")
		}
		identity, err := projectidentity.ResolveCheckout(ctx, directory)
		if err != nil {
			return "", err
		}
		if err := validateWorkspaceCheckoutProject(item, identity); err != nil {
			return "", err
		}
		return identity.Directory, nil
	}
	return "", fmt.Errorf("conversation project %q is unavailable", project)
}

// CreateAndStartPool persists an immutable pool configuration before starting
// its worker engine.
func (m *WorkerManager) CreateAndStartPool(pool storage.WorkerPool) (*storage.WorkerPool, error) {
	if pool.Server == "" {
		pool.Server = m.client.Endpoint()
	}
	if pool.ID == "" {
		pool.ID = uuid.NewString()
	}
	gen, ctx, cancel, err := m.reserve(&pool)
	if err != nil {
		return nil, err
	}
	if pool.Server != m.client.Endpoint() {
		err = fmt.Errorf("worker pool server must match the configured gateway")
		m.failStart(gen, ctx, err)
		return nil, err
	}
	created, err := m.store.CreateWorkerPool(ctx, pool)
	if err != nil {
		m.failStart(gen, ctx, err)
		return nil, err
	}
	loaded, err := m.validateSavedPool(ctx, created.ID)
	if err != nil {
		m.failStartWithPool(gen, ctx, err, created)
		return cloneWorkerPool(created), err
	}
	if err := ctx.Err(); err != nil {
		m.failStartWithPool(gen, ctx, err, loaded)
		return cloneWorkerPool(loaded), err
	}
	if err := m.launch(gen, loaded, ctx, cancel); err != nil {
		return cloneWorkerPool(loaded), err
	}
	return cloneWorkerPool(loaded), nil
}

// ResumePool loads the immutable saved configuration and starts its durable
// worker engine under the manager lifetime.
func (m *WorkerManager) ResumePool(savedID string) error {
	gen, ctx, cancel, err := m.reserve(&storage.WorkerPool{ID: savedID})
	if err != nil {
		return err
	}
	pool, err := m.validateSavedPool(ctx, savedID)
	if err != nil {
		m.failStartWithPool(gen, ctx, err, nil)
		return err
	}
	if err := ctx.Err(); err != nil {
		m.failStartWithPool(gen, ctx, err, pool)
		return err
	}
	return m.launch(gen, pool, ctx, cancel)
}

func (m *WorkerManager) reserve(pool *storage.WorkerPool) (uint64, context.Context, context.CancelFunc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closed:
		return 0, nil, nil, fmt.Errorf("worker manager is closed")
	case !m.started:
		return 0, nil, nil, fmt.Errorf("worker manager has not been started")
	case m.active:
		return 0, nil, nil, fmt.Errorf("worker manager already has an active pool")
	case m.ctx.Err() != nil:
		return 0, nil, nil, m.ctx.Err()
	}
	m.generation++
	ctx, cancel := context.WithCancel(m.ctx)
	m.active = true
	m.poolStop = cancel
	m.current = &workerManagerRun{generation: m.generation, done: make(chan struct{})}
	m.snapshot = WorkerManagerSnapshot{State: WorkerManagerStarting, Pool: cloneWorkerPool(pool), Generation: m.generation}
	return m.generation, ctx, cancel, nil
}

func (m *WorkerManager) validateSavedPool(ctx context.Context, id string) (*storage.WorkerPool, error) {
	pool, err := m.store.WorkerPool(ctx, id)
	if err != nil {
		return nil, err
	}
	if pool.Server != m.client.Endpoint() {
		return nil, fmt.Errorf("saved worker pool belongs to another gateway")
	}
	directory, err := providerprocess.ValidateDirectory(pool.Directory, pool.Provider)
	if err != nil || directory != pool.Directory {
		return nil, fmt.Errorf("saved worker pool checkout is unavailable")
	}
	slots, err := m.store.WorkerPoolSlots(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(slots) != pool.SlotCount || len(slots) == 0 {
		return nil, fmt.Errorf("saved worker pool has an invalid slot count")
	}
	return cloneWorkerPool(pool), nil
}

func (m *WorkerManager) launch(gen uint64, pool *storage.WorkerPool, ctx context.Context, cancel context.CancelFunc) error {
	m.mu.Lock()
	var err error
	switch {
	case m.generation != gen:
		err = fmt.Errorf("worker pool start was superseded")
	case m.closed:
		err = fmt.Errorf("worker manager is closed")
	case ctx.Err() != nil:
		err = ctx.Err()
	}
	if err != nil {
		m.mu.Unlock()
		cancel()
		m.failStartWithPool(gen, ctx, err, pool)
		return err
	}
	m.snapshot.State = WorkerManagerRunning
	m.snapshot.Pool = cloneWorkerPool(pool)
	m.mu.Unlock()
	go func() {
		err := worker.RunPool(ctx, worker.PoolConfig{Store: m.store, Client: m.client, PoolID: pool.ID, Open: m.open, Archive: m.archive, StreamAssignments: m.useTaskStream})
		lifetimeErr := ctx.Err()
		cancel()
		m.finish(gen, lifetimeErr, err)
	}()
	return nil
}

func (m *WorkerManager) failStart(gen uint64, ctx context.Context, err error) {
	m.failStartWithPool(gen, ctx, err, nil)
}

func (m *WorkerManager) failStartWithPool(gen uint64, ctx context.Context, err error, pool *storage.WorkerPool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.generation || !m.active || m.current == nil || m.current.generation != gen {
		return
	}
	if m.poolStop != nil {
		m.poolStop()
	}
	m.active = false
	m.poolStop = nil
	m.current.err = err
	if pool != nil {
		m.snapshot.Pool = cloneWorkerPool(pool)
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		m.current.err = nil
		m.snapshot.State = WorkerManagerStopped
		m.snapshot.Error = ""
	} else if err != nil {
		m.snapshot.State = WorkerManagerFailed
		m.snapshot.Error = err.Error()
	} else {
		m.snapshot.State = WorkerManagerStopped
		m.snapshot.Error = ""
	}
	close(m.current.done)
}

func (m *WorkerManager) finish(gen uint64, lifetimeErr error, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.generation || !m.active || m.current == nil || m.current.generation != gen {
		return
	}
	m.active = false
	m.poolStop = nil
	if lifetimeErr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		err = nil
	}
	m.current.err = err
	if err != nil {
		m.snapshot.State = WorkerManagerFailed
		m.snapshot.Error = err.Error()
	} else {
		m.snapshot.State = WorkerManagerStopped
		m.snapshot.Error = ""
	}
	close(m.current.done)
}

// Snapshot returns an immutable copy of the current manager state.
func (m *WorkerManager) Snapshot() WorkerManagerSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.snapshot
	snapshot.Pool = cloneWorkerPool(snapshot.Pool)
	return snapshot
}

// Stop cancels and joins the active pool while leaving the manager available
// for a later start under its original lifetime context.
func (m *WorkerManager) Stop() {
	m.mu.Lock()
	if m.active {
		m.snapshot.State = WorkerManagerStopping
		if m.poolStop != nil {
			m.poolStop()
		}
	}
	var done <-chan struct{}
	if m.current != nil {
		done = m.current.done
	}
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}

// Wait joins the current or most recently started pool without cancelling it.
func (m *WorkerManager) Wait() error {
	m.mu.Lock()
	current := m.current
	m.mu.Unlock()
	if current == nil {
		return nil
	}
	<-current.done
	return current.err
}

// Close prevents new pool runs, cancels the manager lifetime, and joins the
// active pool before returning.
func (m *WorkerManager) Close() error {
	m.mu.Lock()
	m.closed = true
	if m.active {
		m.snapshot.State = WorkerManagerStopping
		if m.poolStop != nil {
			m.poolStop()
		}
	}
	cancel, streamDone := m.cancel, m.taskStreamDone
	var done <-chan struct{}
	if m.current != nil {
		done = m.current.done
	}
	if !m.active && m.snapshot.State == WorkerManagerIdle {
		m.snapshot.State = WorkerManagerStopped
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if streamDone != nil {
		<-streamDone
	}
	return m.Wait()
}

func cloneWorkerPool(pool *storage.WorkerPool) *storage.WorkerPool {
	if pool == nil {
		return nil
	}
	copy := *pool
	copy.Command = append([]string(nil), pool.Command...)
	if pool.Goal != nil {
		goal := *pool.Goal
		copy.Goal = &goal
	}
	return &copy
}
