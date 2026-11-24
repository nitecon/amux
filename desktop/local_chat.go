package desktop

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/chat"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

const chatRecord = "<!-- amux:"

type localChatFile struct {
	Info        storage.Conversation
	Directory   string
	Preferences ConversationLaunchPreferences
}

type localChat struct {
	file      localChatFile
	path      string
	open      worker.Open
	mu        sync.Mutex
	connectMu sync.Mutex
	events    []storage.ConversationEvent
	wake      chan struct{}
	lifetime  context.Context
	session   worker.Session
	active    string
	cancel    context.CancelFunc
	turns     sync.WaitGroup
}

func projectChatDirectory(root, project string) (string, error) {
	if project == "" || project == "." || project == ".." || projects.SanitizeIdent(project) != project || filepath.Base(project) != project {
		return "", fmt.Errorf("invalid local conversation project")
	}
	return filepath.Join(root, project), nil
}

func markdownRecord(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []byte(chatRecord + base64.RawStdEncoding.EncodeToString(raw) + " -->\n"), nil
}

func readMarkdownChat(path string, withEvents bool) (localChatFile, []storage.ConversationEvent, error) {
	var header localChatFile
	var events []storage.ConversationEvent
	file, err := os.Open(path)
	if err != nil {
		return header, nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		record, ok := strings.CutPrefix(scanner.Text(), chatRecord)
		if !ok {
			continue
		}
		record, ok = strings.CutSuffix(record, " -->")
		if !ok {
			continue
		}
		data, err := base64.RawStdEncoding.DecodeString(record)
		if err != nil {
			return header, nil, err
		}
		if header.Info.ID == "" {
			if err := json.Unmarshal(data, &header); err != nil {
				return header, nil, err
			}
			if !withEvents {
				break
			}
			continue
		}
		var event storage.ConversationEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return header, nil, err
		}
		if event.ConversationID != header.Info.ID || event.Sequence != int64(len(events)+1) {
			return header, nil, fmt.Errorf("invalid conversation event sequence")
		}
		if event.Event.Kind == "session_ready" {
			header.Info.NativeSessionID = event.Event.SessionID
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return header, nil, err
	}
	if header.Info.ID == "" {
		return header, nil, fmt.Errorf("missing local conversation metadata")
	}
	return header, events, nil
}

func listMarkdownChats(root, project string) ([]storage.Conversation, error) {
	dir, err := projectChatDirectory(root, project)
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
	var chats []storage.Conversation
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".md")
		if _, err := uuid.Parse(id); err != nil || !strings.HasSuffix(entry.Name(), ".md") || !entry.Type().IsRegular() {
			continue
		}
		header, _, err := readMarkdownChat(filepath.Join(dir, entry.Name()), false)
		if err != nil {
			return nil, fmt.Errorf("read conversation %s: %w", id, err)
		}
		if header.Info.ID != id || header.Info.ProjectIdent != project {
			continue
		}
		if info, err := entry.Info(); err == nil {
			header.Info.UpdatedAt = info.ModTime().UnixMilli()
		}
		chats = append(chats, header.Info)
	}
	return chats, nil
}

func createMarkdownChat(root, project, directory, provider string, prefs ConversationLaunchPreferences) (storage.Conversation, error) {
	dir, err := projectChatDirectory(root, project)
	if err != nil {
		return storage.Conversation{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return storage.Conversation{}, err
	}
	id := uuid.NewString()
	now := time.Now().UnixMilli()
	info := storage.Conversation{ID: id, ProjectIdent: project, Provider: provider, CreatedAt: now, UpdatedAt: now}
	err = writeMarkdownChat(filepath.Join(dir, id+".md"), localChatFile{Info: info, Directory: directory, Preferences: prefs})
	return info, err
}

func writeMarkdownChat(path string, header localChatFile) error {
	record, err := markdownRecord(header)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append([]byte("# "+providerConversationLabel(header.Info.Provider)+" conversation\n\n"), record...))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func openMarkdownChat(root string, info storage.Conversation, open worker.Open) (*localChat, error) {
	dir, err := projectChatDirectory(root, info.ProjectIdent)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(info.ID); err != nil || filepath.Base(info.ID) != info.ID {
		return nil, fmt.Errorf("invalid local conversation ID")
	}
	path := filepath.Join(dir, info.ID+".md")
	header, events, err := readMarkdownChat(path, true)
	if err != nil {
		return nil, err
	}
	if header.Info.ID != info.ID || header.Info.ProjectIdent != info.ProjectIdent {
		return nil, fmt.Errorf("local conversation metadata does not match its path")
	}
	return &localChat{file: header, path: path, open: open, events: events, wake: make(chan struct{}, 1)}, nil
}

func (c *localChat) Conversation(context.Context, string, string) (*storage.Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := c.file.Info
	return &info, nil
}

func (*localChat) Task(context.Context, string, string) (*storage.Task, error) { return nil, nil }

func (c *localChat) Stream(ctx context.Context, _, _ string, cursor gatewayclient.Cursor, consume func(gatewayclient.Frame) error) error {
	c.mu.Lock()
	if c.lifetime == nil {
		c.lifetime = ctx
	}
	c.mu.Unlock()
	defer func() {
		if ctx.Err() == nil {
			return
		}
		c.mu.Lock()
		c.mu.Unlock()
		c.turns.Wait()
		c.stop()
	}()
	if err := consume(gatewayclient.Frame{Type: "local_connecting"}); err != nil {
		return err
	}
	_, err := c.connect(ctx)
	frame := gatewayclient.Frame{Type: "local_ready"}
	if err != nil {
		frame.Type = "local_failed"
		frame.Event = &storage.ConversationEvent{Event: conversation.Event{Text: err.Error()}}
	}
	if err := consume(frame); err != nil {
		return err
	}
	for {
		c.mu.Lock()
		next := cursor.Events
		if next < int64(len(c.events)) {
			event := c.events[next]
			if event.Event.Kind == "local_prompt_started" && event.Event.MessageID != c.active {
				event.Event.Kind = "state"
			}
			// Native session IDs can only be learned when a provider starts.
			// The local transcript has its own stable identity.
			event.Event.SessionID = c.file.Info.ID
			c.mu.Unlock()
			if err := consume(gatewayclient.Frame{Type: "event", Event: &event}); err != nil {
				return err
			}
			cursor.Events = event.Sequence
			continue
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.wake:
		}
	}
}

func (c *localChat) appendEvent(_ context.Context, event conversation.Event) error {
	var heading, text string
	switch event.Kind {
	case "user_message":
		heading, text = "You", event.Text
	case "local_error":
		heading, text = "Error", event.Text
	}
	return c.writeEvent(event, heading, text)
}

func (c *localChat) writeEvent(event conversation.Event, heading, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	record := storage.ConversationEvent{ID: uuid.NewString(), ConversationID: c.file.Info.ID, Sequence: int64(len(c.events) + 1), Event: event, CreatedAt: time.Now().UnixMilli()}
	data, err := markdownRecord(record)
	if err != nil {
		return err
	}
	if text != "" {
		text = strings.ReplaceAll(text, "\r", "")
		data = append(data, []byte("\n**"+heading+"**\n\n> "+strings.ReplaceAll(text, "\n", "\n> ")+"\n\n")...)
	}
	file, err := os.OpenFile(c.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	c.events = append(c.events, record)
	if event.Kind == "session_ready" {
		c.file.Info.NativeSessionID = event.SessionID
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

func (c *localChat) stop() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	c.mu.Lock()
	session := c.session
	c.session = nil
	c.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
}

func (c *localChat) Enqueue(ctx context.Context, _, _, id string, cmd conversation.Command) error {
	c.mu.Lock()
	switch cmd.Kind {
	case "prompt":
		if c.lifetime == nil || c.lifetime.Err() != nil || c.active != "" {
			c.mu.Unlock()
			return fmt.Errorf("local conversation is busy or closed")
		}
		turn, cancel := context.WithCancel(c.lifetime)
		c.active, c.cancel = id, cancel
		c.turns.Add(1)
		c.mu.Unlock()
		go c.prompt(turn, cancel, id, cmd.Text)
		return nil
	case "cancel":
		if c.active == cmd.PromptCommandID && c.cancel != nil {
			c.cancel()
		}
		c.mu.Unlock()
		return nil
	case "permission":
		session := c.session
		var request string
		if c.active != "" {
			resolved := make(map[string]bool)
			for i := len(c.events) - 1; i >= 0; i-- {
				event := c.events[i]
				if event.Event.Kind == "local_prompt_started" {
					break
				}
				if event.Event.Kind == "permission_resolved" {
					resolved[event.Event.RequestID] = true
				}
				if event.ID == cmd.PermissionEventID && event.Event.Kind == "permission" && !resolved[event.Event.RequestID] {
					request = event.Event.RequestID
					break
				}
			}
		}
		c.mu.Unlock()
		if session == nil || request == "" {
			return fmt.Errorf("permission request is no longer active")
		}
		if err := session.ResolvePermission(ctx, request, cmd.OptionID); err != nil {
			return err
		}
		return c.appendEvent(ctx, conversation.Event{Kind: "permission_resolved", RequestID: request, Status: "resolved"})
	default:
		c.mu.Unlock()
		return fmt.Errorf("unsupported local conversation command")
	}
}

func (c *localChat) connect(ctx context.Context) (worker.Session, error) {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	session, lifetime, info := c.session, c.lifetime, c.file.Info
	if info.NativeSessionID != "" {
		hasPrompt := false
		for i := len(c.events) - 1; i >= 0; i-- {
			if c.events[i].Event.Kind == "user_message" {
				hasPrompt = true
				break
			}
		}
		if !hasPrompt {
			// Some providers create their resumable record on the first turn.
			info.NativeSessionID = ""
		}
	}
	c.mu.Unlock()
	if session != nil {
		select {
		case <-session.Done():
			_ = session.Close()
			c.mu.Lock()
			c.session = nil
			c.mu.Unlock()
		default:
			return session, nil
		}
	}
	command, err := providerLaunchCommand(info.Provider, c.file.Preferences)
	if err != nil {
		return nil, err
	}
	if len(command) == 0 && info.Provider == "antigravity" {
		command = []string{"agy", "acp", "--stdio"}
	}
	if len(command) > 0 {
		command[0] = localProviderExecutable(command[0])
	}
	run := storage.WorkerRun{Project: info.ProjectIdent, ConversationID: info.ID, Directory: c.file.Directory, Command: command, NativeSessionID: info.NativeSessionID, Owner: storage.AgentOrigin{Provider: info.Provider}}
	session, err = c.open(lifetime, run, c.appendEvent)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = session.Close()
		return nil, err
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	return session, nil
}

// Apps started from the desktop may not inherit a shell's user-local PATH.
func localProviderExecutable(name string) string {
	if _, err := exec.LookPath(name); err == nil {
		return name
	}
	if home, err := os.UserHomeDir(); err == nil {
		file := name
		if runtime.GOOS == "windows" {
			file += ".exe"
		}
		if path, err := exec.LookPath(filepath.Join(home, ".local", "bin", file)); err == nil {
			return path
		}
	}
	return name
}

func (c *localChat) prompt(ctx context.Context, cancel context.CancelFunc, id, text string) {
	defer c.turns.Done()
	defer cancel()
	c.mu.Lock()
	info := c.file.Info
	firstEvent := len(c.events)
	c.mu.Unlock()
	session, err := c.connect(ctx)
	if err == nil {
		err = c.appendEvent(ctx, conversation.Event{Kind: "local_prompt_started", MessageID: id})
	}
	if err == nil {
		err = c.appendEvent(ctx, conversation.Event{Kind: "user_message", MessageID: id, SessionID: info.NativeSessionID, Text: text})
	}
	if err == nil {
		_, err = session.Prompt(ctx, text)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = c.appendEvent(context.Background(), conversation.Event{Kind: "local_error", Text: err.Error()})
	}
	_ = c.writeEvent(conversation.Event{Kind: "turn_completed"}, providerConversationLabel(info.Provider), c.assistantText(firstEvent))
	c.mu.Lock()
	c.active, c.cancel = "", nil
	c.mu.Unlock()
}

func (c *localChat) assistantText(first int) string {
	c.mu.Lock()
	events := append([]storage.ConversationEvent(nil), c.events[first:]...)
	c.mu.Unlock()
	model := chat.New()
	for i, event := range events {
		event.Sequence = int64(i + 1)
		event.Event.SessionID = event.ConversationID
		if err := model.Apply(event); err != nil {
			return ""
		}
	}
	var messages []string
	for _, item := range model.Snapshot().Items {
		if item.Kind == "message" && item.Role == "assistant" {
			messages = append(messages, item.Text)
		}
	}
	return strings.Join(messages, "\n\n")
}

func (b *Browser) createLocalConversation(project, directory, provider, replace string) {
	prefs := b.conversationLaunchPreferences(provider)
	_, err := providerLaunchCommand(provider, prefs)
	if err != nil {
		b.newConversationStatus.SetText(err.Error())
		return
	}
	if directory == "" {
		for _, item := range b.projectItems {
			if item.Ident == project {
				directory = b.projectCheckout(item).Directory
				break
			}
		}
	}
	if directory == "" {
		b.showLocalChatOpenError("Check out this project to start a local chat.")
		return
	}
	key := chatKey(project, provider)
	if b.openingDefaults[key] {
		return
	}
	if b.openingDefaults == nil {
		b.openingDefaults = make(map[string]bool)
	}
	b.openingDefaults[key] = true
	selection, workspace := b.conversationSelection, b.activeWorkspaceID
	if !b.launch(func(ctx context.Context) {
		info, err := createMarkdownChat(b.localConversationsDirectory, project, directory, provider, prefs)
		b.dispatch(func() {
			delete(b.openingDefaults, key)
			if err == nil && replace != "" {
				if local := b.localChatSessions[chatKey(project, replace)]; local != nil {
					go local.stop()
				}
			}
			if project != b.selectedProject || workspace != b.activeWorkspaceID {
				return
			}
			if err != nil {
				if selection == b.conversationSelection && b.selection.ProjectConversation == "" {
					b.showLocalChatOpenError("Create local conversation: " + err.Error())
				} else {
					b.newConversationStatus.SetText("Create local conversation: " + err.Error())
				}
				return
			}
			b.newConversationStatus.SetText("")
			if b.mainView == "Conversations" && selection == b.conversationSelection {
				b.openStandaloneConversation(info)
			}
			b.loadProjectConversations()
		})
	}) {
		delete(b.openingDefaults, key)
	}
}

func (b *Browser) showLocalChatOpenError(message string) {
	b.standaloneChatHost.Objects = []fyne.CanvasObject{widget.NewLabel(message)}
	b.standaloneChatHost.Refresh()
}
