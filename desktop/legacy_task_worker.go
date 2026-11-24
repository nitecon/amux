package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func (m *WorkerManager) runLegacyTaskWorker(ctx context.Context, pool storage.WorkerPool, drain <-chan struct{}, presence *rustPresence) error {
	slots, err := m.store.WorkerPoolSlots(ctx, pool.ID)
	if err != nil {
		return err
	}
	if len(slots) != 1 || m.taskConversationRoot == "" {
		return fmt.Errorf("legacy task worker requires one slot and a local transcript directory")
	}
	owner := slots[0].Owner
	host, _ := os.Hostname()
	register := func() error {
		return worker.RegisterTaskTarget(ctx, m.client, pool.Project, pool.Directory, owner, &host)
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
		changes := m.store.WorkerTaskChanges()
		if active == nil {
			if draining {
				return nil
			}
			select {
			case <-drain:
				return nil
			default:
			}
			pending, err := m.store.PendingWorkerTask(ctx, pool.Server, pool.Project, owner)
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
						err = m.store.ForgetWorkerTask(workerCtx, pool.Server, pool.Project, pending)
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

func (m *WorkerManager) executeLegacyTask(ctx context.Context, pool storage.WorkerPool, owner storage.AgentOrigin, id string) error {
	task, err := m.client.StartOwnedTask(ctx, pool.Project, id, owner)
	if err != nil || task == nil {
		return err
	}
	if task.StartedAt == nil {
		return fmt.Errorf("owned task %s has no start timestamp", id)
	}
	claim := strconv.FormatInt(*task.StartedAt, 10)
	info := storage.Conversation{
		ID:           legacyTaskConversationID(id, owner.SessionID),
		ProjectIdent: pool.Project, TaskID: id, TaskTitle: task.Title, Provider: owner.Provider,
		Owner: owner, CreatedAt: time.Now().UnixMilli(),
	}
	run := storage.WorkerRun{Server: pool.Server, Project: pool.Project, ConversationID: info.ID, LocalTaskID: id, Directory: pool.Directory, Command: pool.Command, Owner: owner, Goal: task.Goal}
	archive, err := openTaskConversationArchive(m.taskConversationRoot, info, run)
	if err != nil {
		return err
	}
	for _, event := range archive.events {
		if event.Event.Kind == "local_prompt_started" && event.Event.MessageID == claim {
			return nil
		}
	}
	open := m.open
	if open == nil {
		open = worker.OpenNative
	}
	run.NativeSessionID = archive.file.Info.NativeSessionID
	session, err := open(ctx, run, archive.appendEvent)
	if err != nil {
		return err
	}
	defer session.Close()
	current, err := m.client.OwnedActiveTask(ctx, pool.Project, id, owner)
	if err != nil || current == nil {
		return err
	}
	if current.Status != "in_progress" || current.StartedAt == nil || *current.StartedAt != *task.StartedAt {
		return nil
	}
	prompt := storage.AssignmentPrompt(current)
	if err := archive.appendEvent(ctx, conversation.Event{Kind: "local_prompt_started", MessageID: claim}); err != nil {
		return err
	}
	if err := archive.appendEvent(ctx, conversation.Event{Kind: "user_message", MessageID: claim, Text: prompt}); err != nil {
		return err
	}
	archive.mu.Lock()
	first := len(archive.events)
	archive.mu.Unlock()
	reply, promptErr := session.Prompt(ctx, prompt)
	if reply == "" {
		reply = archive.assistantText(first)
	}
	if promptErr != nil && !errors.Is(promptErr, context.Canceled) {
		if err := archive.appendEvent(ctx, conversation.Event{Kind: "local_error", Text: promptErr.Error()}); err != nil {
			return err
		}
	}
	return archive.writeEvent(conversation.Event{Kind: "turn_completed"}, providerConversationLabel(owner.Provider), reply)
}
