package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func archiveTaskWorker(root string, client *gatewayclient.Client) worker.EventArchive {
	return func(ctx context.Context, run storage.WorkerRun) (conversation.Sink, error) {
		info, err := client.Conversation(ctx, run.Project, run.ConversationID)
		if err != nil {
			return nil, fmt.Errorf("locate conversation for local archive: %w", err)
		}
		if info.TaskID == "" {
			return nil, nil
		}
		archive, err := openTaskConversationArchive(root, *info, run)
		if err != nil {
			return nil, fmt.Errorf("open local task transcript: %w", err)
		}
		first := len(archive.events)
		return func(ctx context.Context, event conversation.Event) error {
			if event.Kind == "turn_completed" {
				err := archive.writeEvent(event, providerConversationLabel(info.Provider), archive.assistantText(first))
				if err == nil {
					first = len(archive.events)
				}
				return err
			}
			return archive.appendEvent(ctx, event)
		}, nil
	}
}

func openTaskConversationArchive(root string, info storage.Conversation, run storage.WorkerRun) (*localChat, error) {
	dir, err := taskTranscriptDirectory(root, info.ProjectIdent, info.TaskID)
	if err != nil {
		return nil, err
	}
	if !canonicalTaskUUID(run.Owner.SessionID) {
		return nil, fmt.Errorf("invalid agent identity")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, run.Owner.SessionID+".md")
	header := localChatFile{Info: info, Directory: run.Directory}
	if err := writeMarkdownChat(path, header); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	header, events, err := readMarkdownChat(path, true)
	if err != nil {
		return nil, err
	}
	if header.Info.ID != info.ID || header.Info.ProjectIdent != info.ProjectIdent || header.Info.TaskID != info.TaskID {
		return nil, fmt.Errorf("task transcript belongs to a different conversation")
	}
	return &localChat{file: header, path: path, events: events, wake: make(chan struct{}, 1)}, nil
}
