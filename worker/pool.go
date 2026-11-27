package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
	"github.com/rs/zerolog/log"
)

// PoolConfig connects a saved native worker pool to its gateway and local
// provider runtime. The caller holds the host profile lock for RunPool's life.
type PoolConfig struct {
	Observe           EventObserver
	Archive           EventArchive
	Drain             <-chan struct{}
	StreamAssignments bool
	Store             *storage.Store
	Client            *gatewayclient.Client
	PoolID            string
	Open              Open
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	// ClientRun identifies this worker engine lifetime across transport retries.
	ClientRun string
}

var poolGuards sync.Map

// RunPool drives every saved slot using the same durable worker engine as
// Run. Slot state is the recovery record: active runs resume in place, while
// idle slots may adopt one server assignment at a time.
func RunPool(parent context.Context, config PoolConfig) error {
	if config.Store == nil || config.Client == nil || config.PoolID == "" {
		return fmt.Errorf("worker pool requires local journal, gateway and pool ID")
	}
	if _, loaded := poolGuards.LoadOrStore(config.PoolID, struct{}{}); loaded {
		return fmt.Errorf("worker pool %q is already running in this process", config.PoolID)
	}
	defer poolGuards.Delete(config.PoolID)
	if config.ClientRun == "" {
		config.ClientRun = uuid.NewString()
	} else if parsed, err := uuid.Parse(config.ClientRun); err != nil || parsed.String() != config.ClientRun {
		return fmt.Errorf("worker client run must be a canonical UUID")
	}
	if config.Open == nil {
		config.Open = OpenNative
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 2 * time.Second
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 15 * time.Second
	}
	pool, err := config.Store.WorkerPool(parent, config.PoolID)
	if err != nil {
		return err
	}
	if pool.Server != config.Client.Endpoint() {
		return fmt.Errorf("worker pool belongs to another gateway")
	}
	slots, err := config.Store.WorkerPoolSlots(parent, config.PoolID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var group sync.WaitGroup
	poolDone := make(chan struct{})
	results := make(chan error, len(slots))
	for _, slot := range slots {
		slot := slot
		group.Add(1)
		go func() {
			defer group.Done()
			if err := runPoolSlot(ctx, config, *pool, slot); err != nil && ctx.Err() == nil {
				select {
				case results <- fmt.Errorf("worker pool slot %s: %w", slot.ID, err):
				default:
				}
				cancel()
			}
		}()
	}
	go func() { group.Wait(); close(poolDone) }()
	var result error
	select {
	case result = <-results:
	case <-parent.Done():
	case <-poolDone:
		select {
		case result = <-results:
		default:
		}
	}
	cancel()
	group.Wait()
	return result
}

func poolPresence(ctx context.Context, config PoolConfig, pool storage.WorkerPool, slot storage.WorkerPoolSlot, busy bool) error {
	current, err := config.Store.NextWorkerPoolReport(ctx, pool.ID, slot.ID)
	if err != nil {
		return err
	}
	activity := current.LastActivityAt
	var last *int64
	if activity != 0 {
		last = &activity
	}
	// Every adopted task has a new conversation/run and starts a fresh native
	// session, including fallback reviews by a prior contributor/provider.
	machineOS, hostname := poolMachineMetadata()
	entry := storage.PresenceAgent{ProjectIdent: pool.Project, Origin: current.Owner, Generation: &current.Owner.InstanceID, Hostname: hostname, Goal: pool.Goal, Busy: busy, AutoAssign: !busy, ExecutionIdle: !busy, LastActivityAt: last, ReviewContextClear: true}
	report := storage.PresenceReport{Source: pool.ID + "/" + slot.ID, Sequence: current.ReportSequence, OS: machineOS, Hostname: hostname, Agents: []storage.PresenceAgent{entry}}
	if config.ClientRun != "" {
		report.ClientRun = &config.ClientRun
	}
	return retry(ctx, func() error {
		return registerAndReportPresence(ctx, config.Client, pool.Project, pool.Directory, current.Owner, hostname, report)
	})
}

func poolMachineMetadata() (string, *string) {
	return workerMachineMetadata()
}

func runPoolSlot(ctx context.Context, config PoolConfig, pool storage.WorkerPool, initial storage.WorkerPoolSlot) error {
	slot := initial
	var activeCancel context.CancelFunc
	var activeDone chan error
	activeTask := ""
	var followUpDone chan error
	var followUpCancel context.CancelFunc
	defer func() {
		if activeCancel != nil {
			activeCancel()
			<-activeDone
		}
		if followUpDone != nil {
			followUpCancel()
			<-followUpDone
		}
		omitPoolSlot(config, pool, slot)
	}()
	if err := poolPresence(ctx, config, pool, slot, slot.RunID != ""); err != nil && ctx.Err() == nil {
		return err
	}
	poll := time.NewTicker(config.PollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(config.HeartbeatInterval)
	defer heartbeat.Stop()
	ready := make(chan struct{})
	close(ready)
	var scan, changes <-chan struct{}
	if config.StreamAssignments {
		scan = ready
		changes = config.Store.WorkerTaskChanges()
	}
	drain := config.Drain
	for {
		polled := false
		select {
		case <-ctx.Done():
			return nil
		case <-scan:
			scan = nil
		case <-drain:
			drain = nil
		case <-changes:
			changes = config.Store.WorkerTaskChanges()
			if activeCancel != nil {
				continue
			}
		case err := <-activeDone:
			activeDone = nil
			activeCancel = nil
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return err // Keep the run journal intact for exact recovery.
			}
			return fmt.Errorf("saved worker run ended unexpectedly")
		case err := <-followUpDone:
			followUpCancel()
			followUpDone = nil
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				log.Warn().Str("project", pool.Project).Str("session", slot.Owner.SessionID).Err(err).Msg("task conversation follow-up stopped")
			}
			select {
			case <-config.Drain:
				return nil
			default:
			}
			if err := poolPresence(ctx, config, pool, slot, false); err != nil {
				return err
			}
			scan = ready
			continue
		case <-heartbeat.C:
			fresh, err := poolSlot(ctx, config.Store, pool.ID, slot.ID)
			if err != nil {
				return err
			}
			slot = fresh
			if err = poolPresence(ctx, config, pool, slot, slot.RunID != "" || followUpDone != nil); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			continue
		case <-poll.C:
			polled = true
		}
		if followUpDone != nil {
			continue
		}
		{
			fresh, err := poolSlot(ctx, config.Store, pool.ID, slot.ID)
			if err != nil {
				return err
			}
			slot = fresh
			if activeCancel != nil {
				if err := checkPoolTask(ctx, config, &slot, activeTask); err != nil {
					if errors.Is(err, errPoolTaskFinished) {
						activeCancel()
						if workerErr := <-activeDone; workerErr != nil && ctx.Err() == nil {
							return workerErr
						}
						activeCancel, activeDone = nil, nil
						if err = retry(ctx, func() error { return Flush(ctx, config.Store, config.Client, slot.RunID) }); err != nil {
							return err
						}
						if err = config.Store.FinishWorkerPoolRun(ctx, pool.ID, slot.ID, slot.RunID, time.Now().UTC()); err != nil {
							return err
						}
						slot.RunID = ""
						if config.StreamAssignments {
							scan = ready
						}
						activeTask = ""
						select {
						case <-config.Drain:
							return nil
						default:
						}
						if err = poolPresence(ctx, config, pool, slot, false); err != nil {
							return err
						}
						continue
					}
					return err
				}
				continue
			}
			if slot.RunID != "" {
				run, err := config.Store.WorkerRun(ctx, slot.RunID)
				if err != nil {
					return err
				}
				conv, err := retryValue(ctx, func() (*storage.Conversation, error) {
					return config.Client.Conversation(ctx, run.Project, run.ConversationID)
				})
				if err != nil {
					return err
				}
				activeTask = conv.TaskID
				if err = checkPoolTask(ctx, config, &slot, activeTask); err != nil {
					if errors.Is(err, errPoolTaskFinished) {
						if err = retry(ctx, func() error { return Flush(ctx, config.Store, config.Client, run.ID) }); err != nil {
							return err
						}
						if err = config.Store.FinishWorkerPoolRun(ctx, pool.ID, slot.ID, run.ID, time.Now().UTC()); err != nil {
							return err
						}
						slot.RunID, activeTask = "", ""
						if config.StreamAssignments {
							scan = ready
						}
						select {
						case <-config.Drain:
							return nil
						default:
						}
						if err = poolPresence(ctx, config, pool, slot, false); err != nil {
							return err
						}
						continue
					}
					return err
				}
				activeCtx, stop := context.WithCancel(ctx)
				activeCancel = stop
				activeDone = make(chan error, 1)
				go func() {
					activeDone <- Run(activeCtx, Config{Store: config.Store, Client: config.Client, RunID: run.ID, Open: config.Open, Archive: config.Archive, Observe: config.Observe})
				}()
				continue
			}
			select {
			case <-config.Drain:
				return nil
			default:
			}
			if polled {
				id, err := pendingPoolFollowUp(ctx, config, pool, slot)
				if err != nil {
					log.Warn().Str("project", pool.Project).Err(err).Msg("could not check task conversation follow-ups")
				}
				if id != "" {
					if err := poolPresence(ctx, config, pool, slot, true); err != nil {
						return err
					}
					runCtx, stop := context.WithCancel(ctx)
					followUpCancel = stop
					followUpDone = make(chan error, 1)
					done := followUpDone
					go func() {
						done <- Run(runCtx, Config{Store: config.Store, Client: config.Client, RunID: id, Open: config.Open, Archive: config.Archive, Observe: config.Observe, followUp: true})
					}()
					continue
				}
				if config.StreamAssignments {
					continue
				}
			}
			var pending storage.PendingWorkerTask
			if config.StreamAssignments {
				pending, err = config.Store.PendingWorkerTask(ctx, pool.Server, pool.Project, slot.Owner)
				if err != nil {
					return err
				}
				if pending.TaskID == "" {
					continue
				}
			}
			adopt := func() (*storage.WorkerAssignment, error) {
				return config.Client.AdoptWorkerTask(ctx, pool.Project, pending.TaskID, slot.Owner, pool.Goal)
			}
			var assignment *storage.WorkerAssignment
			if config.StreamAssignments {
				assignment, err = retryValue(ctx, adopt)
			} else {
				assignment, err = adopt()
			}
			if err != nil {
				if gatewayclient.Retryable(err) {
					continue
				}
				return err
			}
			if assignment == nil {
				if pending.TaskID != "" {
					if err := config.Store.ForgetWorkerTask(ctx, pool.Server, pool.Project, pending); err != nil {
						return err
					}
					scan = ready
				}
				continue
			}
			run := storage.WorkerRun{ID: assignment.RunID, Server: config.Client.Endpoint(), Project: pool.Project, ConversationID: assignment.Conversation.ID, Directory: pool.Directory, Command: append([]string(nil), pool.Command...), Owner: assignment.Conversation.Owner, Goal: pool.Goal}
			if err = config.Store.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
				return err
			}
			if pending.TaskID != "" {
				if err := config.Store.ForgetWorkerTask(ctx, pool.Server, pool.Project, pending); err != nil {
					return err
				}
			}
			slot.RunID = run.ID
			if err = poolPresence(ctx, config, pool, slot, true); err != nil {
				return err
			}
			activeTask = assignment.Task.ID
			activeCtx, stop := context.WithCancel(ctx)
			activeCancel = stop
			activeDone = make(chan error, 1)
			go func() {
				activeDone <- Run(activeCtx, Config{Store: config.Store, Client: config.Client, RunID: run.ID, Open: config.Open, Archive: config.Archive, Observe: config.Observe})
			}()
		}
	}
}

// Only resume conversations with a recovery journal on this host. A slot may
// have processed other tasks since this run, so its current instance is only
// used for discovery. Run takes over from the saved conversation instance.
func pendingPoolFollowUp(ctx context.Context, config PoolConfig, pool storage.WorkerPool, slot storage.WorkerPoolSlot) (string, error) {
	ids, err := config.Client.PendingTaskConversations(ctx, pool.Project, slot.Owner)
	var response *gatewayclient.HTTPError
	if errors.As(err, &response) && response.Status == 404 {
		return "", nil // Gateway predates task conversation follow-ups.
	}
	if err != nil {
		return "", err
	}
	for _, conversationID := range ids {
		id, err := config.Store.ConversationWorkerRunID(ctx, pool.Server, pool.Project, conversationID)
		if err != nil {
			return "", err
		}
		if id == "" {
			continue
		}
		run, err := config.Store.WorkerRun(ctx, id)
		if err != nil {
			return "", err
		}
		if run.NativeSessionID != "" && run.Owner.SessionID == slot.Owner.SessionID &&
			run.Directory == pool.Directory && run.Owner.Provider == pool.Provider && run.Owner.OS == pool.OS {
			return id, nil
		}
	}
	return "", nil
}

var errPoolTaskFinished = errors.New("worker pool task is no longer owned and active")

func checkPoolTask(ctx context.Context, config PoolConfig, slot *storage.WorkerPoolSlot, taskID string) error {
	return checkRunTask(ctx, config.Store, config.Client, slot.RunID, taskID)
}

func checkRunTask(ctx context.Context, store *storage.Store, client *gatewayclient.Client, runID, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("saved worker conversation has no task ID")
	}
	before, err := store.WorkerRun(ctx, runID)
	if err != nil {
		return err
	}
	task, err := retryValue(ctx, func() (*storage.Task, error) { return client.Task(ctx, before.Project, taskID) })
	if err != nil {
		return err
	}
	// Reload after the remote read. Run may have rotated the conversation owner
	// while Task was in flight, so compare against the latest durable instance.
	run, err := store.WorkerRun(ctx, runID)
	if err != nil {
		return err
	}
	if before.Owner != run.Owner {
		return nil
	}
	owned := func(owner *storage.AgentOrigin, expected storage.AgentOrigin) bool {
		return owner != nil && *owner == expected
	}
	pendingOwner := run.Owner
	if run.PendingInstanceID != "" {
		pendingOwner.InstanceID = run.PendingInstanceID
	}
	if !storage.IsActiveTaskStatus(task.Status) || !owned(task.OwnerOrigin, run.Owner) && !(run.PendingInstanceID != "" && owned(task.OwnerOrigin, pendingOwner)) {
		return errPoolTaskFinished
	}
	return nil
}

func retryValue[T any](ctx context.Context, operation func() (T, error)) (T, error) {
	var value T
	err := retry(ctx, func() error {
		var err error
		value, err = operation()
		return err
	})
	return value, err
}

func poolSlot(ctx context.Context, store *storage.Store, poolID, slotID string) (storage.WorkerPoolSlot, error) {
	slots, err := store.WorkerPoolSlots(ctx, poolID)
	if err != nil {
		return storage.WorkerPoolSlot{}, err
	}
	for _, slot := range slots {
		if slot.ID == slotID {
			return slot, nil
		}
	}
	return storage.WorkerPoolSlot{}, fmt.Errorf("worker pool slot %q disappeared", slotID)
}

func omitPoolSlot(config PoolConfig, pool storage.WorkerPool, slot storage.WorkerPoolSlot) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reportSlot, err := config.Store.NextWorkerPoolReport(ctx, pool.ID, slot.ID)
	if err != nil {
		return
	}
	machineOS, hostname := poolMachineMetadata()
	report := storage.PresenceReport{Source: pool.ID + "/" + slot.ID, Sequence: reportSlot.ReportSequence, OS: machineOS, Hostname: hostname}
	if config.ClientRun != "" {
		report.ClientRun = &config.ClientRun
	}
	for ctx.Err() == nil {
		_, err = config.Client.ReportPresence(ctx, report)
		if err == nil || !gatewayclient.Retryable(err) {
			return
		}
		if pause(ctx, 100*time.Millisecond) != nil {
			return
		}
	}
}
