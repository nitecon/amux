package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func legacyTaskConversationID(task, agent string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(task+"/"+agent)).String()
}

func taskTranscriptDirectory(root, project, task string) (string, error) {
	dir, err := projectChatDirectory(root, project)
	if err != nil {
		return "", err
	}
	if !canonicalTaskUUID(task) {
		return "", fmt.Errorf("invalid task identity")
	}
	return filepath.Join(dir, "tasks", task), nil
}

func canonicalTaskUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func listLocalTaskThreads(root, project, task string) ([]storage.Conversation, error) {
	dir, err := taskTranscriptDirectory(root, project, task)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []storage.Conversation
	for _, entry := range entries {
		agent, ok := strings.CutSuffix(entry.Name(), ".md")
		if !ok || !entry.Type().IsRegular() || !canonicalTaskUUID(agent) {
			continue
		}
		header, _, err := readMarkdownChat(filepath.Join(dir, entry.Name()), false)
		if err != nil {
			return nil, err
		}
		if header.Info.TaskID == task && header.Info.ProjectIdent == project && header.Info.Owner.SessionID == agent {
			result = append(result, header.Info)
		}
	}
	return result, nil
}

type localTaskThread struct {
	info storage.Conversation
	path string
}

func (c localTaskThread) Conversation(context.Context, string, string) (*storage.Conversation, error) {
	return &c.info, nil
}

func (localTaskThread) Task(context.Context, string, string) (*storage.Task, error) { return nil, nil }

func (localTaskThread) Enqueue(context.Context, string, string, string, conversation.Command) error {
	return fmt.Errorf("task transcript is read-only")
}

func (c localTaskThread) Stream(ctx context.Context, _, _ string, cursor gatewayclient.Cursor, consume func(gatewayclient.Frame) error) error {
	if err := consume(gatewayclient.Frame{Type: "local_transcript"}); err != nil {
		return err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var modified time.Time
	var size int64 = -1
	for {
		stat, err := os.Stat(c.path)
		if err != nil {
			return err
		}
		if stat.Size() != size || !stat.ModTime().Equal(modified) {
			header, events, err := readMarkdownChat(c.path, true)
			if err != nil {
				return err
			}
			if header.Info.ID != c.info.ID || header.Info.TaskID != c.info.TaskID || header.Info.ProjectIdent != c.info.ProjectIdent {
				return fmt.Errorf("task transcript metadata does not match its path")
			}
			for _, event := range events {
				if event.Sequence <= cursor.Events {
					continue
				}
				if event.Event.Kind == "local_prompt_started" {
					event.Event.Kind = "state"
				}
				event.Event.SessionID = c.info.ID
				if err := consume(gatewayclient.Frame{Type: "event", Event: &event}); err != nil {
					return err
				}
				cursor.Events = event.Sequence
			}
			modified, size = stat.ModTime(), stat.Size()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
