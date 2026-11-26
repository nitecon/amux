package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/chat"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

// Config identifies a remote conversation. Dispatch is mainly useful to make
// widget updates deterministic in tests; normal applications use fyne.Do.
type conversationClient interface {
	Conversation(context.Context, string, string) (*storage.Conversation, error)
	Task(context.Context, string, string) (*storage.Task, error)
	Stream(context.Context, string, string, gatewayclient.Cursor, func(gatewayclient.Frame) error) error
	Enqueue(context.Context, string, string, string, conversation.Command) error
}

type Config struct {
	Client         conversationClient
	Project        string
	ConversationID string
	TaskTitle      string
	ReadOnly       bool
	Model          *chat.Model
	Dispatch       func(func())
	// ToolApprovalsPath is the shared preferences file that stores
	// "Always allow" rules. Empty hides the option.
	ToolApprovalsPath     string
	ToolApprovalDirectory string
}

type approvalRuleDraft struct {
	pattern, directory string
}

type UI struct {
	client         conversationClient
	project        string
	conversationID string
	model          *chat.Model
	dispatch       func(func())

	root                    *fyne.Container
	transcript              *fyne.Container
	scroll                  *container.Scroll
	connection              *widget.Label
	heading                 *widget.Label
	composer                *conversationComposer
	sendButton              *widget.Button
	cancelButton            *widget.Button
	status                  *widget.Label
	toolButtons             map[string]*widget.Button
	permissionButtons       map[string]*widget.Button
	permissionDetailButtons map[string]*widget.Button
	expandedPermissions     map[string]bool
	toolApprovalsPath       string
	toolApprovalDirectory   string
	approvalRuleDrafts      map[string]approvalRuleDraft
	transcriptEnd           int
	refreshPending          atomic.Bool

	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	cursor        gatewayclient.Cursor
	pendingPrompt string
	pendingText   string
	activePrompt  string
	started       bool
	closing       bool
	wg            sync.WaitGroup
}

func New(config Config) (*UI, error) {
	if config.Client == nil || strings.TrimSpace(config.Project) == "" || strings.TrimSpace(config.ConversationID) == "" {
		return nil, fmt.Errorf("desktop chat requires a gateway client, project and conversation")
	}
	if config.Model == nil {
		config.Model = chat.New()
	}
	dispatch := config.Dispatch
	if dispatch == nil {
		dispatch = fyne.Do
	}
	u := &UI{
		client: config.Client, project: config.Project, conversationID: config.ConversationID,
		model: config.Model, dispatch: dispatch, transcriptEnd: -1, toolButtons: map[string]*widget.Button{}, permissionButtons: map[string]*widget.Button{}, permissionDetailButtons: map[string]*widget.Button{}, expandedPermissions: map[string]bool{},
		toolApprovalsPath: config.ToolApprovalsPath, toolApprovalDirectory: config.ToolApprovalDirectory, approvalRuleDrafts: map[string]approvalRuleDraft{},
		heading: selectableLabel(config.TaskTitle), connection: widget.NewLabel("Ready to connect"),
		transcript: container.NewVBox(), status: widget.NewLabel(""),
	}
	if strings.TrimSpace(config.TaskTitle) == "" {
		u.heading.SetText(config.Project + " · " + shortID(config.ConversationID))
	}
	u.ctx, u.cancel = context.WithCancel(context.Background())
	u.heading.TextStyle = fyne.TextStyle{Bold: true}
	u.composer = &conversationComposer{Entry: widget.Entry{MultiLine: true}, submit: u.Submit}
	u.composer.ExtendBaseWidget(u.composer)
	u.composer.SetPlaceHolder("Message the agent…")
	u.composer.Wrapping = fyne.TextWrapWord
	u.composer.OnChanged = func(text string) {
		u.model.SetDraft(text)
		u.updateButtons()
	}
	u.sendButton = widget.NewButton("Send", u.Submit)
	u.cancelButton = widget.NewButton("Cancel current prompt", u.CancelPrompt)
	u.sendButton.Importance = widget.HighImportance
	u.transcript.Add(widget.NewLabel("Conversation events will appear here."))
	u.scroll = container.NewVScroll(u.transcript)
	composerRow := container.NewBorder(nil, nil, nil, container.NewHBox(u.cancelButton, u.sendButton), u.composer)
	footer := container.NewVBox(u.status)
	if !config.ReadOnly {
		footer.Add(composerRow)
	}
	u.root = container.NewBorder(container.NewVBox(u.heading, u.connection, widget.NewSeparator()), footer, nil, nil, u.scroll)
	u.refresh()
	u.updateButtons()
	return u, nil
}

func (u *UI) Content() fyne.CanvasObject { return u.root }

// Start performs metadata lookup and subscribes to the durable event stream.
func (u *UI) Start(parent context.Context) {
	u.mu.Lock()
	if u.started || u.closing {
		u.mu.Unlock()
		return
	}
	u.started = true
	if u.cancel != nil {
		u.cancel()
	}
	u.ctx, u.cancel = context.WithCancel(parent)
	u.mu.Unlock()
	u.launch(func(ctx context.Context) { u.loadConversation(ctx) })
	u.launch(func(ctx context.Context) { u.stream(ctx) })
}

func (u *UI) Close() {
	u.mu.Lock()
	u.closing = true
	u.started = false
	cancel := u.cancel
	u.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Wait drains background network and command goroutines after Close.
func (u *UI) Wait() { u.wg.Wait() }

func (u *UI) launch(run func(context.Context)) {
	u.mu.Lock()
	if u.closing {
		u.mu.Unlock()
		return
	}
	ctx := u.ctx
	u.wg.Add(1)
	u.mu.Unlock()
	go func() {
		defer u.wg.Done()
		run(ctx)
	}()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (u *UI) loadConversation(ctx context.Context) {
	conversationInfo, err := u.client.Conversation(ctx, u.project, u.conversationID)
	if err != nil {
		if ctx.Err() == nil {
			u.setStatus("Conversation details unavailable: " + err.Error())
		}
		return
	}
	if conversationInfo.ProjectIdent != u.project || conversationInfo.ID != u.conversationID {
		u.setStatus("Gateway returned a different conversation.")
		return
	}
	if conversationInfo.WorkingDirectory != "" {
		u.dispatch(func() {
			u.toolApprovalDirectory = conversationInfo.WorkingDirectory
			u.refresh()
		})
	}
	if conversationInfo.TaskID == "" {
		return
	}
	task, err := u.client.Task(ctx, u.project, conversationInfo.TaskID)
	if err != nil {
		if ctx.Err() == nil {
			u.setStatus("Task details unavailable: " + err.Error())
		}
		return
	}
	if task != nil {
		title := strings.TrimSpace(task.Title)
		if title == "" {
			title = u.project
		}
		title += " · " + shortID(u.conversationID)
		u.dispatch(func() { u.heading.SetText(title) })
	}
}

func (u *UI) stream(ctx context.Context) {
	delay := 200 * time.Millisecond
	for ctx.Err() == nil {
		sequence := u.model.Sequence()
		u.mu.Lock()
		cursor := u.cursor
		cursor.Events = sequence
		u.mu.Unlock()
		u.setConnection("Connecting…")
		err := u.client.Stream(ctx, u.project, u.conversationID, cursor, u.consume)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !gatewayclient.Retryable(err) {
			u.setConnection("Connection stopped")
			u.setStatus("Conversation stream: " + err.Error())
			return
		}
		u.setConnection("Reconnecting…")
		if !wait(ctx, delay) {
			return
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}

func (u *UI) consume(frame gatewayclient.Frame) error {
	switch frame.Type {
	case "local_transcript":
		u.setConnection("Local task transcript")
	case "local_connecting":
		u.setConnection("Connecting to local provider…")
	case "local_ready":
		u.setConnection("Local provider connected")
		u.setStatus("")
	case "local_failed":
		u.setConnection("Local provider unavailable")
		if frame.Event != nil {
			u.setStatus(frame.Event.Event.Text)
		}
	case "ready":
		u.setConnection("Gateway connected")
	case "owner":
		u.model.ClearPermissions()
		u.mu.Lock()
		u.activePrompt = ""
		u.mu.Unlock()
		u.setStatus("Agent changed; pending approvals cleared.")
		u.dispatch(func() { clear(u.expandedPermissions); u.refresh(); u.updateButtons() })
	case "event":
		if frame.Event == nil {
			return fmt.Errorf("gateway sent an empty event frame")
		}
		if frame.Event.Event.Kind == "local_error" {
			u.setStatus(frame.Event.Event.Text)
		}
		if err := u.model.Apply(*frame.Event); err != nil {
			return err
		}
		u.mu.Lock()
		u.cursor.Events = frame.Event.Sequence
		if frame.Event.Event.Kind == "local_prompt_started" {
			u.activePrompt = frame.Event.Event.MessageID
			u.setConnection("Local provider connected")
		}
		if frame.Event.Event.Kind == "turn_completed" || frame.Event.Event.Kind == "owner_handoff" {
			u.activePrompt = ""
		}
		u.mu.Unlock()
		u.queueTranscriptRefresh()
	case "command":
		if frame.Command == nil {
			return fmt.Errorf("gateway sent an empty command frame")
		}
		u.mu.Lock()
		u.cursor.Commands = frame.Command.Revision
		if frame.Command.Command.Kind == "prompt" {
			if (frame.Command.State == "claimed" || frame.Command.State == "running") && (u.activePrompt == "" || u.activePrompt == frame.Command.ID) {
				u.activePrompt = frame.Command.ID
			} else if frame.Command.State != "queued" && u.activePrompt == frame.Command.ID {
				u.activePrompt = ""
			}
		}
		u.mu.Unlock()
		u.dispatch(u.updateButtons)
	}
	return nil
}

type conversationComposer struct {
	widget.Entry
	submit func()
}

func (e *conversationComposer) TypedKey(key *fyne.KeyEvent) {
	var modifiers fyne.KeyModifier
	if driver, ok := fyne.CurrentApp().Driver().(desktop.Driver); ok {
		modifiers = driver.CurrentKeyModifiers()
	}
	e.typedKey(key, modifiers)
}

func (e *conversationComposer) typedKey(key *fyne.KeyEvent, modifiers fyne.KeyModifier) {
	if (key.Name == fyne.KeyReturn || key.Name == fyne.KeyEnter) && modifiers == 0 {
		if !e.Disabled() {
			e.submit()
		}
		return
	}
	e.Entry.TypedKey(key)
}

func (u *UI) Submit() {
	u.mu.Lock()
	if !u.started {
		u.mu.Unlock()
		return
	}
	if u.pendingPrompt != "" {
		u.mu.Unlock()
		return
	}
	draftText := u.model.Draft()
	draft := strings.TrimSpace(draftText)
	if draft == "" {
		u.mu.Unlock()
		return
	}
	id := uuid.NewString()
	u.pendingPrompt, u.pendingText = id, draftText
	u.mu.Unlock()
	u.setStatus("Sending prompt…")
	u.dispatch(u.updateButtons)
	u.launch(func(ctx context.Context) { u.enqueue(ctx, id, conversation.Command{Kind: "prompt", Text: draft}, true) })
}

func (u *UI) CancelPrompt() {
	u.mu.Lock()
	if !u.started {
		u.mu.Unlock()
		return
	}
	id := u.activePrompt
	u.mu.Unlock()
	if id == "" {
		return
	}
	u.setStatus("Cancelling prompt…")
	commandID := uuid.NewString()
	u.launch(func(ctx context.Context) {
		u.enqueue(ctx, commandID, conversation.Command{Kind: "cancel", PromptCommandID: id}, false)
	})
}

func (u *UI) choosePermission(permission chat.Permission, optionID string) {
	u.mu.Lock()
	if !u.started {
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()
	commandID := uuid.NewString()
	u.launch(func(ctx context.Context) {
		u.enqueue(ctx, commandID, conversation.Command{Kind: "permission", PermissionEventID: permission.EventID, OptionID: optionID}, false)
	})
}

func (u *UI) enqueue(ctx context.Context, id string, command conversation.Command, prompt bool) {
	delay := 200 * time.Millisecond
	for ctx.Err() == nil {
		err := u.client.Enqueue(ctx, u.project, u.conversationID, id, command)
		if err == nil {
			if prompt {
				u.mu.Lock()
				text := u.pendingText
				u.pendingPrompt, u.pendingText = "", ""
				u.mu.Unlock()
				if u.model.Draft() == text {
					u.model.SetDraft("")
					u.dispatch(func() { u.composer.SetText(""); u.refresh(); u.updateButtons() })
				} else {
					u.dispatch(u.updateButtons)
				}
			}
			u.setStatus("")
			u.dispatch(u.updateButtons)
			return
		}
		if !gatewayclient.Retryable(err) {
			u.setStatus("Command not accepted: " + err.Error())
			if prompt {
				u.mu.Lock()
				u.pendingPrompt = ""
				u.mu.Unlock()
				u.dispatch(u.updateButtons)
			}
			return
		}
		u.setConnection("Reconnecting…")
		if !wait(ctx, delay) {
			return
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (u *UI) queueTranscriptRefresh() {
	if u.refreshPending.Swap(true) {
		return
	}
	u.dispatch(func() {
		u.refreshPending.Store(false)
		u.refresh()
		u.updateButtons()
	})
}

const conversationPageSize = 40

func (u *UI) refresh() {
	u.refreshTranscript(true)
}

func (u *UI) refreshTranscript(follow bool) {
	snapshot, start, total := u.model.TranscriptWindow(u.transcriptEnd, conversationPageSize)
	u.toolButtons = map[string]*widget.Button{}
	u.permissionButtons = map[string]*widget.Button{}
	u.permissionDetailButtons = map[string]*widget.Button{}
	objects := make([]fyne.CanvasObject, 0, len(snapshot.Items)+len(snapshot.Permissions)+1)
	if start > 0 {
		objects = append(objects, widget.NewButton("Older messages", func() {
			u.transcriptEnd = start
			u.refreshTranscript(false)
			u.scroll.ScrollToTop()
		}))
	}
	for _, item := range snapshot.Items {
		if rendered := u.renderItem(item); rendered != nil {
			objects = append(objects, rendered)
		}
	}
	if end := start + len(snapshot.Items); end < total {
		objects = append(objects, widget.NewButton("Newer messages", func() {
			u.transcriptEnd = end + conversationPageSize
			if u.transcriptEnd >= u.modelItemCount() {
				u.transcriptEnd = -1
			}
			u.refreshTranscript(true)
		}))
	}
	for _, permission := range snapshot.Permissions {
		objects = append(objects, u.renderPermission(permission))
	}
	if len(objects) == 0 {
		objects = append(objects, widget.NewLabel("No messages yet. Start with a prompt below."))
	}
	u.transcript.Objects = objects
	u.transcript.Refresh()
	u.scroll.Refresh()
	if follow && u.transcriptEnd < 0 {
		u.scroll.ScrollToBottom()
	}
}

func (u *UI) modelItemCount() int {
	_, _, total := u.model.TranscriptWindow(0, 0)
	return total
}

func (u *UI) renderItem(item chat.Item) fyne.CanvasObject {
	switch item.Kind {
	case "tool":
		label := item.Title
		if item.Command != "" {
			label = item.Command
		}
		if label == "" {
			label = "Tool call"
		}
		label = toolPreview(label)
		if item.Status != "" {
			label += " · " + item.Status
		}
		caption := "Inspect details"
		if item.Expanded {
			caption = "Hide details"
		}
		button := widget.NewButton(caption, func() {
			_ = u.model.SetExpanded(item.ToolCallID, !item.Expanded)
			u.dispatch(func() { u.refreshTranscript(false) })
		})
		button.Importance = widget.LowImportance
		u.toolButtons[item.ToolCallID] = button
		preview := selectableLabel(label)
		preview.Truncation = fyne.TextTruncateEllipsis
		if item.Command != "" {
			preview.TextStyle = fyne.TextStyle{Monospace: true}
		}
		row := container.NewBorder(nil, nil, selectableLabel("Tool"), button, preview)
		if item.Expanded {
			var details any
			text := string(item.Details)
			if json.Unmarshal(item.Details, &details) == nil {
				if pretty, err := json.MarshalIndent(details, "", "  "); err == nil {
					text = string(pretty)
				}
			}
			detail := selectableLabel(text)
			detail.Wrapping = fyne.TextWrapWord
			detail.TextStyle = fyne.TextStyle{Monospace: true}
			expanded := container.NewVBox(row)
			if item.Command != "" {
				command := selectableLabel(item.Command)
				command.Wrapping = fyne.TextWrapWord
				command.TextStyle = fyne.TextStyle{Monospace: true}
				expanded.Add(command)
			}
			if item.Text != "" {
				output := selectableLabel(item.Text)
				output.Wrapping = fyne.TextWrapWord
				output.TextStyle = fyne.TextStyle{Monospace: true}
				expanded.Add(output)
			}
			if item.Text == "" {
				expanded.Add(detail)
			} else {
				expanded.Add(widget.NewAccordion(widget.NewAccordionItem("Provider details", detail)))
			}
			return expanded
		}
		return row
	case "state":
		if item.Text == "" && item.Title == "" &&
			!strings.Contains(strings.ToLower(item.Status), "error") &&
			!strings.Contains(strings.ToLower(item.Status), "failed") {
			return nil
		}
		text := item.Text
		if text == "" {
			text = item.Title
		}
		if text == "" {
			text = item.Status
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return selectableLabel("•  " + text)
	default:
		if strings.TrimSpace(item.Text) == "" {
			return nil
		}
		role := item.Role
		if role == "" {
			role = "Assistant"
		} else {
			role = strings.ToUpper(role[:1]) + role[1:]
		}
		header := selectableLabel(role)
		header.TextStyle = fyne.TextStyle{Bold: true}
		body := selectableLabel(item.Text)
		body.Wrapping = fyne.TextWrapWord
		if item.Kind == "reasoning" {
			body.TextStyle = fyne.TextStyle{Italic: true}
		}
		if item.Status != "" {
			return container.NewVBox(header, body, selectableLabel(item.Status))
		}
		return container.NewVBox(header, body)
	}
}

func toolPreview(text string) string {
	const limit = 80
	oneLine := strings.Join(strings.Fields(text), " ")
	chars := []rune(oneLine)
	if len(chars) <= limit {
		return oneLine
	}
	return string(chars[:limit-1]) + "…"
}

func (u *UI) renderPermission(permission chat.Permission) fyne.CanvasObject {
	title := permission.Title
	if title == "" {
		title = "Agent requests permission"
	}
	buttons := make([]fyne.CanvasObject, 0, len(permission.Options))
	for _, option := range permission.Options {
		option := option
		name := option.Name
		if name == "" {
			name = option.ID
		}
		button := widget.NewButton(name, func() { u.choosePermission(permission, option.ID) })
		u.permissionButtons[permission.EventID+"/"+option.ID] = button
		buttons = append(buttons, button)
	}
	inspectText := "Inspect request details"
	if u.expandedPermissions[permission.EventID] {
		inspectText = "Hide request details"
	}
	inspect := widget.NewButton(inspectText, func() {
		u.expandedPermissions[permission.EventID] = !u.expandedPermissions[permission.EventID]
		u.dispatch(u.refresh)
	})
	u.permissionDetailButtons[permission.EventID] = inspect
	content := []fyne.CanvasObject{container.NewHBox(buttons...), inspect}
	content = append(content, u.renderApprovalRule(permission)...)
	if u.expandedPermissions[permission.EventID] {
		var details any
		text := string(permission.Details)
		if text == "" {
			text = "No structured request details were provided."
		}
		if json.Unmarshal(permission.Details, &details) == nil {
			if pretty, err := json.MarshalIndent(details, "", "  "); err == nil {
				text = string(pretty)
			}
		}
		detail := selectableLabel(text)
		detail.Wrapping = fyne.TextWrapWord
		detail.TextStyle = fyne.TextStyle{Monospace: true}
		content = append(content, detail)
	}
	return widget.NewCard("Permission required", title, container.NewVBox(content...))
}

// renderApprovalRule offers an editable "Always allow" rule prefilled with the
// requested command, so the user can widen it (for example to "git grep *")
// before saving it and approving this request.
func (u *UI) renderApprovalRule(permission chat.Permission) []fyne.CanvasObject {
	event := conversation.Event{Data: permission.Details}
	subject := worksubmission.ToolApprovalSubject(event)
	directory := worksubmission.ToolApprovalDirectory(event, u.toolApprovalDirectory)
	allow := ""
	for _, option := range permission.Options {
		if option.Kind == "allow_once" {
			allow = option.ID
			break
		}
	}
	if u.toolApprovalsPath == "" || subject == "" || allow == "" {
		return nil
	}
	draft, editing := u.approvalRuleDrafts[permission.EventID]
	if !editing {
		return []fyne.CanvasObject{widget.NewButton("Always allow…", func() {
			u.approvalRuleDrafts[permission.EventID] = approvalRuleDraft{worksubmission.ToolApprovalRuleDraft(subject), directory}
			u.dispatch(u.refresh)
		})}
	}
	entry := widget.NewEntry()
	entry.SetText(draft.pattern)
	entry.OnChanged = func(text string) {
		draft := u.approvalRuleDrafts[permission.EventID]
		draft.pattern = text
		u.approvalRuleDrafts[permission.EventID] = draft
	}
	directoryEntry := widget.NewEntry()
	directoryEntry.SetPlaceHolder("Absolute directory for this rule")
	directoryEntry.SetText(draft.directory)
	directoryEntry.OnChanged = func(text string) {
		draft := u.approvalRuleDrafts[permission.EventID]
		draft.directory = text
		u.approvalRuleDrafts[permission.EventID] = draft
	}
	save := widget.NewButton("Save rule and allow", func() {
		rule, path, directory := entry.Text, u.toolApprovalsPath, directoryEntry.Text
		u.launch(func(context.Context) {
			if !worksubmission.MatchToolApproval([]string{rule}, subject) {
				u.dispatch(func() { u.status.SetText("Approval rule must match this request before it can be saved.") })
				return
			}
			if err := worksubmission.AddDirectoryToolApproval(path, directory, rule); err != nil {
				u.dispatch(func() { u.status.SetText("Approval rule was not saved: " + err.Error()) })
				return
			}
			u.dispatch(func() {
				delete(u.approvalRuleDrafts, permission.EventID)
				u.choosePermission(permission, allow)
			})
		})
	})
	cancel := widget.NewButton("Cancel", func() {
		delete(u.approvalRuleDrafts, permission.EventID)
		u.dispatch(u.refresh)
	})
	hint := widget.NewLabel("Automatically allow matching requests in this directory. * matches any text; \\* is a literal *. Edit the command pattern before saving.")
	hint.Wrapping = fyne.TextWrapWord
	return []fyne.CanvasObject{hint, widget.NewForm(widget.NewFormItem("Directory", directoryEntry), widget.NewFormItem("Command or tool", entry)), container.NewHBox(save, cancel)}
}

func (u *UI) updateButtons() {
	u.mu.Lock()
	pending, active, started := u.pendingPrompt != "", u.activePrompt != "", u.started
	u.mu.Unlock()
	u.sendButton.Disable()
	if started && !pending && strings.TrimSpace(u.model.Draft()) != "" {
		u.sendButton.Enable()
	}
	if started && active {
		u.cancelButton.Enable()
	} else {
		u.cancelButton.Disable()
	}
}

func (u *UI) setConnection(text string) { u.dispatch(func() { u.connection.SetText(text) }) }
func (u *UI) setStatus(text string)     { u.dispatch(func() { u.status.SetText(text) }) }

// Snapshot exposes the thread-safe model view for host integrations and tests.
func (u *UI) Snapshot() chat.Snapshot { return u.model.Snapshot() }

// ReportStatus lets a host surface a local worker failure in the conversation UI.
func (u *UI) ReportStatus(message string) { u.setStatus(message) }
