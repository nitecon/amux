package localconversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func ArchiveTaskWorker(root string, client *gatewayclient.Client) worker.EventArchive {
	return func(ctx context.Context, run storage.WorkerRun) (conversation.Sink, error) {
		info, err := client.Conversation(ctx, run.Project, run.ConversationID)
		if err != nil {
			return nil, fmt.Errorf("locate conversation for local archive: %w", err)
		}
		if info.TaskID == "" {
			return nil, nil
		}
		archive, err := OpenTaskArchive(root, *info, run)
		if err != nil {
			return nil, fmt.Errorf("open local task transcript: %w", err)
		}
		return archiveSink(archive, info.Provider), nil
	}
}

func ArchiveFleetWorker(root string, client *gatewayclient.Client) worker.EventArchive {
	return func(ctx context.Context, run storage.WorkerRun) (conversation.Sink, error) {
		info, err := client.Conversation(ctx, run.Project, run.ConversationID)
		if err != nil {
			return nil, err
		}
		if info.FleetHostname == nil || !CanonicalUUID(info.ID) {
			return nil, fmt.Errorf("invalid fleet conversation")
		}
		dir, err := ProjectDirectory(root, info.ProjectIdent)
		if err != nil {
			return nil, err
		}
		archive, err := openArchive(filepath.Join(dir, "fleet"), info.ID, *info, run)
		if err != nil {
			return nil, err
		}
		return archiveSink(archive, info.Provider), nil
	}
}

func archiveSink(archive *Session, provider string) conversation.Sink {
	first := len(archive.events)
	return func(ctx context.Context, event conversation.Event) error {
		if event.Kind == "turn_completed" {
			err := archive.WriteEvent(event, ProviderLabel(provider), archive.AssistantText(first))
			if err == nil {
				first = len(archive.events)
			}
			return err
		}
		return archive.AppendEvent(ctx, event)
	}
}

func OpenTaskArchive(root string, info storage.Conversation, run storage.WorkerRun) (*Session, error) {
	dir, err := TaskDirectory(root, info.ProjectIdent, info.TaskID)
	if err != nil {
		return nil, err
	}
	if !CanonicalUUID(run.Owner.SessionID) {
		return nil, fmt.Errorf("invalid agent identity")
	}
	return openArchive(dir, run.Owner.SessionID, info, run)
}

func openArchive(dir, filename string, info storage.Conversation, run storage.WorkerRun) (*Session, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filename+".md")
	header := Header{Info: info, Directory: run.Directory}
	if err := writeMarkdownChat(path, header); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	header, events, err := Read(path, true)
	if err != nil {
		return nil, err
	}
	if header.Info.ProjectIdent != info.ProjectIdent || header.Info.TaskID != info.TaskID || header.Info.Owner.SessionID != run.Owner.SessionID || header.Info.Provider != info.Provider {
		return nil, fmt.Errorf("transcript belongs to a different project, task, or agent")
	}
	return &Session{file: header, path: path, events: events, wake: make(chan struct{})}, nil
}

func TaskDirectory(root, project, task string) (string, error) {
	dir, err := ProjectDirectory(root, project)
	if err != nil {
		return "", err
	}
	if !CanonicalUUID(task) {
		return "", fmt.Errorf("invalid task identity")
	}
	return filepath.Join(dir, "tasks", task), nil
}

func CanonicalUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func LegacyTaskID(task, agent string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(task+"/"+agent)).String()
}
