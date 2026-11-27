// Package worker runs the same durable native-agent engine for headless and
// desktop hosts. It owns provider lifetime; a disconnected UI is not its owner.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/providers/acp"
	"github.com/nitecon/amux/providers/claude"
	"github.com/nitecon/amux/providers/codex"
	"github.com/nitecon/amux/storage"
)

type Session interface {
	Done() <-chan struct{}
	Err() error
	Prompt(context.Context, string) (string, error)
	ResolvePermission(context.Context, string, string) error
	Close() error
}

// Open transfers ownership only on success. On error it must clean up any
// partial provider resources; Run will not call methods on the failed result.
type Open func(context.Context, storage.WorkerRun, conversation.Sink) (Session, error)

// EventArchive attaches a local transcript to a worker run.
type EventArchive func(context.Context, storage.WorkerRun) (conversation.Sink, error)

// EventObserver receives journal events after the gateway acknowledges them.
type EventObserver func(storage.WorkerRun, string, conversation.Event)

type Config struct {
	Observe           EventObserver
	Archive           EventArchive
	Store             *storage.Store
	Client            *gatewayclient.Client
	RunID             string
	Open              Open
	followUp          bool
	AdvertisePresence bool
}

func OpenACP(ctx context.Context, r storage.WorkerRun, sink conversation.Sink) (Session, error) {
	if r.Owner.Provider != "antigravity" {
		return nil, fmt.Errorf("provider adapter is not implemented")
	}
	return acp.Start(ctx, acp.Config{Directory: r.Directory, ResumeID: r.NativeSessionID, Command: r.Command}, sink)
}

// OpenNative selects the provider persisted with the worker run. Keeping this
// decision on the saved origin makes restart independent of CLI defaults.
func OpenNative(ctx context.Context, r storage.WorkerRun, sink conversation.Sink) (Session, error) {
	switch r.Owner.Provider {
	case "antigravity":
		return OpenACP(ctx, r, sink)
	case "codex":
		return codex.Start(ctx, codex.Config{Directory: r.Directory, ResumeID: r.NativeSessionID, Command: r.Command}, sink)
	case "claude":
		return claude.Start(ctx, claude.Config{Directory: r.Directory, ResumeID: r.NativeSessionID, Command: r.Command}, sink)
	default:
		return nil, fmt.Errorf("provider %q is not implemented", r.Owner.Provider)
	}
}

// Flush publishes the local ordered outbox using the persisted origin. Call
// before instance takeover so previously recorded output cannot become stale.
func Flush(ctx context.Context, store *storage.Store, client *gatewayclient.Client, runID string) error {
	return flush(ctx, store, client, runID, nil)
}

func flush(ctx context.Context, store *storage.Store, client *gatewayclient.Client, runID string, observe EventObserver) error {
	for {
		r, err := store.WorkerRun(ctx, runID)
		if err != nil {
			return err
		}
		if r.Server != client.Endpoint() {
			return fmt.Errorf("worker journal belongs to another gateway")
		}
		entries, err := store.WorkerOutbox(ctx, runID, 100)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			sequence, err := client.Publish(ctx, *r, entry)
			if err != nil {
				return err
			}
			if err = store.AckWorkerOutbox(ctx, runID, entry.Sequence, sequence); err != nil {
				return err
			}
			if observe != nil && entry.Kind == "event" {
				var event conversation.Event
				if err := json.Unmarshal(entry.Payload, &event); err == nil {
					observe(*r, entry.ID, event)
				}
			}
		}
	}
}
func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func retry(ctx context.Context, operation func() error) error {
	delay := 100 * time.Millisecond
	for {
		err := operation()
		if err == nil || !gatewayclient.Retryable(err) {
			return err
		}
		if err = pause(ctx, delay); err != nil {
			return err
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}
func Run(parent context.Context, config Config) (runErr error) {
	if config.Store == nil || config.Client == nil || config.RunID == "" {
		return fmt.Errorf("worker requires local journal, gateway and run ID")
	}
	release, err := config.Store.AcquireWorkerRun(config.RunID)
	if err != nil {
		return err
	}
	defer release()
	if config.Open == nil {
		config.Open = OpenNative
	}
	if err := retry(parent, func() error { return Flush(parent, config.Store, config.Client, config.RunID) }); err != nil {
		return err
	}
	planned, err := config.Store.PrepareWorkerInstance(parent, config.RunID)
	if err != nil {
		return err
	}
	if config.followUp && planned.NativeSessionID == "" {
		return fmt.Errorf("follow-up requires the saved native session")
	}
	owner := planned.Owner
	owner.InstanceID = planned.PendingInstanceID
	if err = retry(parent, func() error { return config.Client.Resume(parent, *planned, owner) }); err != nil {
		return err
	}
	if err = config.Store.ActivateWorkerInstance(parent, config.RunID, owner.InstanceID); err != nil {
		return err
	}
	r, err := config.Store.WorkerRun(parent, config.RunID)
	if err != nil {
		return err
	}
	if config.Observe != nil {
		defer config.Observe(*r, "", conversation.Event{Kind: "closed"})
	}
	clientRun := ""
	if config.AdvertisePresence {
		clientRun = uuid.NewString()
	}
	ctx, cancelRun := context.WithCancelCause(parent)
	stop := func() { cancelRun(nil) }
	var group sync.WaitGroup
	var session Session
	defer func() {
		stop()
		if session != nil {
			session.Close()
		}
		group.Wait()
		if config.followUp && parent.Err() == nil {
			// Publish the final result while this engine still owns the journal.
			if err := retry(parent, func() error { return flush(parent, config.Store, config.Client, r.ID, config.Observe) }); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
		if config.AdvertisePresence {
			omitRunPresence(config.Client, config.Store, r.ID, clientRun)
		}
	}()
	if config.AdvertisePresence {
		current, report, hostname, reportErr := nextRunPresence(parent, config.Store, r.ID, clientRun, false)
		if reportErr != nil {
			return reportErr
		}
		if reportErr = retry(parent, func() error { return sendRunPresence(parent, config.Client, current, report, hostname) }); reportErr != nil {
			return reportErr
		}
	}
	failures := make(chan error, 3)
	launch := func(fn func() error) {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := fn(); err != nil && ctx.Err() == nil {
				failures <- err
				stop()
			}
		}()
	}
	if config.AdvertisePresence && !config.followUp {
		launch(func() error {
			taskID := r.LocalTaskID
			if taskID == "" {
				conv, err := retryValue(ctx, func() (*storage.Conversation, error) {
					return config.Client.Conversation(ctx, r.Project, r.ConversationID)
				})
				if err != nil {
					return err
				}
				taskID = conv.TaskID
			}
			if taskID == "" {
				return nil
			}
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
				}
				err := checkRunTask(ctx, config.Store, config.Client, r.ID, taskID)
				if errors.Is(err, errPoolTaskFinished) {
					cancelRun(errPoolTaskFinished)
					return nil
				}
				if err != nil {
					return err
				}
			}
		})
	}
	var archive conversation.Sink
	if config.Archive != nil {
		archive, err = config.Archive(ctx, *r)
		if err != nil {
			return err
		}
	}
	var mu sync.Mutex
	pending := map[string]string{}
	sink := func(_ context.Context, event conversation.Event) error {
		// Local commits remain available during network loss and bounded shutdown.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		mu.Lock()
		defer mu.Unlock()
		id, err := config.Store.QueueWorkerEvent(saveCtx, r.ID, event)
		if err != nil {
			return err
		}
		if archive != nil {
			if err := archive(saveCtx, event); err != nil {
				return err
			}
		}
		switch event.Kind {
		case "permission":
			for key, request := range pending {
				if request == event.RequestID {
					delete(pending, key)
				}
			}
			pending[id] = event.RequestID
		case "turn_completed":
			clear(pending)
		}
		return nil
	}
	opened, err := config.Open(ctx, *r, sink)
	if err != nil {
		return err
	}
	session = opened
	launch(func() error {
		for {
			changed := config.Store.ConversationChanges()
			if err := retry(ctx, func() error { return flush(ctx, config.Store, config.Client, r.ID, config.Observe) }); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-changed:
			}
		}
	})
	launch(func() error {
		return retry(ctx, func() error {
			current, err := config.Store.WorkerRun(ctx, r.ID)
			if err != nil {
				return err
			}
			return config.Client.Watch(ctx, *current, func(c storage.ConversationCommand) error { return config.Store.ObserveWorkerCommand(ctx, r.ID, c) })
		})
	})
	if config.AdvertisePresence {
		launch(func() error {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
				}
				current, report, hostname, err := nextRunPresence(ctx, config.Store, r.ID, clientRun, false)
				if err != nil {
					return err
				}
				err = retry(ctx, func() error { return sendRunPresence(ctx, config.Client, current, report, hostname) })
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					// A permanent binding/validation error stops further admission
					// while leaving the selected provider and its journal running.
					omitRunPresence(config.Client, config.Store, r.ID, clientRun)
					return nil
				}
			}
		})
	}
	type turnResult struct {
		id  string
		err error
	}
	var followUpDeadline <-chan time.Time
	if config.followUp {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		followUpDeadline = timer.C
	}
	turns := make(chan turnResult, 1)
	activeID := ""
	var cancelTurn context.CancelFunc
	defer func() {
		if cancelTurn != nil {
			cancelTurn()
		}
	}()
	for {
		changed := config.Store.ConversationChanges()
		commands, err := config.Store.WorkerCommands(ctx, r.ID)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		var retryDelay <-chan time.Time
		for _, entry := range commands {
			c := entry.Record
			if c.TargetInstanceID != r.Owner.InstanceID || (c.State != "queued" && c.State != "claimed") {
				continue
			}
			if c.Command.Kind == "prompt" && activeID != "" {
				continue
			}
			claimed, err := config.Client.Claim(ctx, *r, c.ID)
			if err != nil {
				var response *gatewayclient.HTTPError
				if gatewayclient.Retryable(err) || errors.As(err, &response) && response.Status == 409 {
					if retryDelay == nil {
						retryDelay = time.After(time.Second)
					}
					continue
				}
				if ctx.Err() != nil {
					break
				}
				return err
			}
			if claimed.State != "claimed" {
				continue
			} // Expired controls are rejected atomically by the server.
			if err = config.Store.StartWorkerCommand(ctx, r.ID, c.ID); err != nil {
				return err
			}
			if c.Command.Kind == "prompt" {
				turnCtx, cancel := context.WithCancel(ctx)
				followUpDeadline = nil
				activeID = c.ID
				cancelTurn = cancel
				group.Add(1)
				go func(id, text string) {
					defer group.Done()
					// Record input before invoking the provider; do not rely on a provider
					// echoing user text in its update stream.
					current, err := config.Store.WorkerRun(turnCtx, r.ID)
					if err == nil {
						err = sink(turnCtx, conversation.Event{Kind: "user_message", SessionID: current.NativeSessionID, MessageID: id, Text: text})
					}
					if err == nil {
						_, err = session.Prompt(turnCtx, text)
					}
					outcome := "done"
					if err != nil {
						outcome = "interrupted"
						if ctx.Err() == nil && errors.Is(err, context.Canceled) {
							outcome = "done"
						}
					}
					saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					saveErr := config.Store.CompleteWorkerCommand(saveCtx, r.ID, id, outcome)
					cancel()
					if saveErr != nil {
						err = saveErr
					}
					turns <- turnResult{id, err}
				}(c.ID, c.Command.Text)
				continue
			}
			outcome := "done"
			switch c.Command.Kind {
			case "permission":
				mu.Lock()
				request, exists := pending[c.Command.PermissionEventID]
				delete(pending, c.Command.PermissionEventID)
				mu.Unlock()
				if !exists || request != c.RequestID {
					outcome = "rejected"
				} else {
					if err = session.ResolvePermission(ctx, request, c.Command.OptionID); err != nil {
						outcome = "rejected"
					} else {
						current, readErr := config.Store.WorkerRun(ctx, r.ID)
						if readErr != nil {
							return readErr
						}
						if err = sink(ctx, conversation.Event{Kind: "permission_resolved", SessionID: current.NativeSessionID, RequestID: request, Status: "resolved"}); err != nil {
							return err
						}
					}
				}
			case "cancel":
				if activeID != c.Command.PromptCommandID || cancelTurn == nil {
					outcome = "rejected"
				} else {
					cancelTurn()
				}
			default:
				outcome = "rejected"
			}
			if err = config.Store.CompleteWorkerCommand(ctx, r.ID, c.ID, outcome); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			select {
			case err := <-failures:
				return err
			default:
			}
			if parent.Err() != nil || errors.Is(context.Cause(ctx), errPoolTaskFinished) {
				return nil
			}
			return fmt.Errorf("worker stopped unexpectedly")
		case err := <-failures:
			return err
		case <-session.Done():
			if parent.Err() != nil || errors.Is(context.Cause(ctx), errPoolTaskFinished) {
				return nil
			}
			if err := session.Err(); err != nil {
				return err
			}
			return fmt.Errorf("native provider session stopped")
		case <-followUpDeadline:
			return nil
		case result := <-turns:
			if result.id == activeID {
				activeID = ""
				if cancelTurn != nil {
					cancelTurn()
					cancelTurn = nil
				}
			}
			// A provider/transport failure ends this instance. Restart only through
			// exact native session recovery; never silently create a fresh session.
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				return result.err
			}
			if config.followUp {
				return nil
			}
		case <-changed:
		case <-retryDelay:
		}
	}
	select {
	case err := <-failures:
		return err
	default:
	}
	if parent.Err() != nil || errors.Is(context.Cause(ctx), errPoolTaskFinished) {
		return nil
	}
	return ctx.Err()
}
