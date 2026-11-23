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
)

// PoolConfig connects a saved native worker pool to its gateway and local
// provider runtime. The caller holds the host profile lock for RunPool's life.
type PoolConfig struct {
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
	var result error
	select {
	case result = <-results:
	case <-parent.Done():
	}
	cancel()
	group.Wait()
	if result != nil {
		return result
	}
	if parent.Err() != nil {
		return nil
	}
	return nil
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
	defer func() {
		if activeCancel != nil {
			activeCancel()
			<-activeDone
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
	for {
		select {
		case <-ctx.Done():
			return nil
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
		case <-heartbeat.C:
			fresh, err := poolSlot(ctx, config.Store, pool.ID, slot.ID)
			if err != nil {
				return err
			}
			slot = fresh
			if err = poolPresence(ctx, config, pool, slot, slot.RunID != ""); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case <-poll.C:
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
						activeTask = ""
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
					activeDone <- Run(activeCtx, Config{Store: config.Store, Client: config.Client, RunID: run.ID, Open: config.Open})
				}()
				continue
			}
			assignment, err := config.Client.AdoptWorkerAssignmentWithGoal(ctx, pool.Project, slot.Owner, pool.Goal)
			if err != nil {
				if gatewayclient.Retryable(err) {
					continue
				}
				return err
			}
			if assignment == nil {
				continue
			}
			run := storage.WorkerRun{ID: assignment.RunID, Server: config.Client.Endpoint(), Project: pool.Project, ConversationID: assignment.Conversation.ID, Directory: pool.Directory, Command: append([]string(nil), pool.Command...), Owner: assignment.Conversation.Owner, Goal: pool.Goal}
			if err = config.Store.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
				return err
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
				activeDone <- Run(activeCtx, Config{Store: config.Store, Client: config.Client, RunID: run.ID, Open: config.Open})
			}()
		}
	}
}

var errPoolTaskFinished = errors.New("worker pool task is no longer owned and active")

func checkPoolTask(ctx context.Context, config PoolConfig, slot *storage.WorkerPoolSlot, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("saved worker conversation has no task ID")
	}
	before, err := config.Store.WorkerRun(ctx, slot.RunID)
	if err != nil {
		return err
	}
	pool, err := config.Store.WorkerPool(ctx, config.PoolID)
	if err != nil {
		return err
	}
	task, err := retryValue(ctx, func() (*storage.Task, error) { return config.Client.Task(ctx, pool.Project, taskID) })
	if err != nil {
		return err
	}
	// Reload after the remote read. Run may have rotated the conversation owner
	// while Task was in flight, so compare against the latest durable instance.
	run, err := config.Store.WorkerRun(ctx, slot.RunID)
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
	if task.Status != "in_progress" || !owned(task.OwnerOrigin, run.Owner) && !(run.PendingInstanceID != "" && owned(task.OwnerOrigin, pendingOwner)) {
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
