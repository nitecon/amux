package localconversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type LegacyTaskWorker struct {
	Store          *storage.Store
	Client         *gatewayclient.Client
	TranscriptRoot string
	Open           worker.Open
	Approvals      *TaskApprovals
}

func (m LegacyTaskWorker) Run(ctx context.Context, pool storage.WorkerPool, drain <-chan struct{}, presence *RustPresence) error {
	slots, err := m.Store.WorkerPoolSlots(ctx, pool.ID)
	if err != nil {
		return err
	}
	if len(slots) != 1 || m.TranscriptRoot == "" {
		return fmt.Errorf("legacy task worker requires one slot and a local transcript directory")
	}
	owner := slots[0].Owner
	host, _ := os.Hostname()
	register := func() error {
		return worker.RegisterTaskTarget(ctx, m.Client, pool.Project, pool.Directory, owner, &host)
	}
	if err := register(); err != nil {
		return err
	}
	checkout, err := projectidentity.Resolve(ctx, pool.Directory)
	if err != nil {
		return err
	}
	agent := gatewayclient.RustPresenceAgent{AgentOrigin: owner, Name: worker.AgentDisplayName(owner), Project: pool.Project, Generation: owner.InstanceID, Goal: pool.Goal, ReviewClear: true}
	if checkout.Upstream != "" {
		agent.CanonicalRemote = &checkout.Upstream
	}
	presence.set(agent, true)
	defer presence.set(agent, false)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var active <-chan error
	draining := false
	workerCtx, stop := context.WithCancel(ctx)
	defer func() {
		stop()
		if active != nil {
			<-active
		}
	}()
	for {
		changes := m.Store.WorkerTaskChanges()
		if active == nil {
			if draining {
				return nil
			}
			select {
			case <-drain:
				return nil
			default:
			}
			pending, err := m.Store.PendingWorkerTask(ctx, pool.Server, pool.Project, owner)
			if err != nil {
				return err
			}
			if pending.TaskID != "" {
				done := make(chan error, 1)
				active = done
				agent.Busy = true
				presence.set(agent, true)
				go func() {
					err := m.executeLegacyTask(workerCtx, pool, owner, pending.TaskID)
					if err == nil {
						err = m.Store.ForgetWorkerTask(workerCtx, pool.Server, pool.Project, pending)
					}
					done <- err
				}()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-drain:
			draining, drain = true, nil
		case <-changes:
		case err := <-active:
			active = nil
			if draining {
				return err
			}
			select {
			case <-drain:
				return err
			default:
			}
			agent.Busy = false
			presence.set(agent, true)
			if err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := register(); err != nil {
				return err
			}
		}
	}
}

func (m LegacyTaskWorker) executeLegacyTask(ctx context.Context, pool storage.WorkerPool, owner storage.AgentOrigin, id string) error {
	task, err := m.Client.StartOwnedTask(ctx, pool.Project, id, owner)
	if err != nil || task == nil {
		return err
	}
	if task.StartedAt == nil {
		return fmt.Errorf("owned task %s has no start timestamp", id)
	}
	claim := strconv.FormatInt(*task.StartedAt, 10)
	info := storage.Conversation{
		ID:           LegacyTaskID(id, owner.SessionID),
		ProjectIdent: pool.Project, TaskID: id, TaskTitle: task.Title, Provider: owner.Provider,
		Owner: owner, CreatedAt: time.Now().UnixMilli(),
	}
	run := storage.WorkerRun{Server: pool.Server, Project: pool.Project, ConversationID: info.ID, LocalTaskID: id, Directory: pool.Directory, Command: pool.Command, Owner: owner, Goal: task.Goal}
	archive, err := OpenTaskArchive(m.TranscriptRoot, info, run)
	if err != nil {
		return err
	}
	header, previousEvents := archive.Snapshot()
	for _, event := range previousEvents {
		if event.Event.Kind == "local_prompt_started" && event.Event.MessageID == claim {
			return nil
		}
	}
	open := m.Open
	if open == nil {
		open = worker.OpenNative
	}
	run.NativeSessionID = header.Info.NativeSessionID
	var session worker.Session
	ready := make(chan struct{})
	emit := archive.AppendEvent
	if m.Approvals != nil {
		defer m.Approvals.Observe(info, "", conversation.Event{Kind: "closed"}, nil)
		emit = func(eventCtx context.Context, event conversation.Event) error {
			if err := archive.AppendEvent(eventCtx, event); err != nil {
				return err
			}
			m.Approvals.Observe(info, uuid.NewString(), event, func(actionCtx context.Context, option string) error {
				select {
				case <-ready:
					if session == nil {
						return fmt.Errorf("provider is no longer active")
					}
				case <-ctx.Done():
					return ctx.Err()
				case <-actionCtx.Done():
					return actionCtx.Err()
				}
				if err := session.ResolvePermission(actionCtx, event.RequestID, option); err != nil {
					return err
				}
				resolved := conversation.Event{Kind: "permission_resolved", RequestID: event.RequestID, Status: "resolved"}
				m.Approvals.Observe(info, "", resolved, nil)
				return archive.AppendEvent(actionCtx, resolved)
			})
			return nil
		}
	}
	session, err = open(ctx, run, emit)
	close(ready)
	if err != nil {
		return err
	}
	defer session.Close()
	current, err := m.Client.OwnedActiveTask(ctx, pool.Project, id, owner)
	if err != nil || current == nil {
		return err
	}
	if (current.Status != "in_progress" && current.Status != "stalled") || current.StartedAt == nil || *current.StartedAt != *task.StartedAt {
		return nil
	}
	prompt := storage.AssignmentPrompt(current)
	if err := archive.AppendEvent(ctx, conversation.Event{Kind: "local_prompt_started", MessageID: claim}); err != nil {
		return err
	}
	if err := archive.AppendEvent(ctx, conversation.Event{Kind: "user_message", MessageID: claim, Text: prompt}); err != nil {
		return err
	}
	first := archive.EventCount()
	reply, promptErr := session.Prompt(ctx, prompt)
	if reply == "" {
		reply = archive.AssistantText(first)
	}
	if promptErr != nil && !errors.Is(promptErr, context.Canceled) {
		if err := archive.AppendEvent(ctx, conversation.Event{Kind: "local_error", Text: promptErr.Error()}); err != nil {
			return err
		}
	}
	return archive.WriteEvent(conversation.Event{Kind: "turn_completed"}, ProviderLabel(owner.Provider), reply)
}
