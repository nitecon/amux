package desktop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/providers/process"
	"github.com/nitecon/amux/storage"
)

type workerPanelController interface {
	Start(context.Context) error
	CreateAndStartPool(storage.WorkerPool) (*storage.WorkerPool, error)
	ResumePool(string) error
	Snapshot() WorkerManagerSnapshot
	Stop()
	Close() error
	Wait() error
}

type workerPanel struct {
	browser *Browser
	manager workerPanelController

	checkout        *widget.Entry
	cloneSource     *widget.Entry
	discoveryParent *widget.Entry
	discover        *widget.Button
	checkouts       *widget.Select
	recoveryOrigins *widget.Select
	attachCheckout  *widget.Button
	cloneCheckout   *widget.Button
	discoveryStatus *widget.Label
	automaticStatus *widget.Label
	provider        *widget.Select
	launchSummary   *widget.Label
	slots           *widget.Select
	goal            *widget.Entry
	savedID         *widget.Entry
	start           *widget.Button
	resume          *widget.Button
	stop            *widget.Button
	status          *widget.Label
	identity        *widget.Label
	accordion       *widget.Accordion
	root            *container.Split

	started                 bool
	closed                  bool
	busy                    bool
	op                      uint64
	startError              string
	requestError            string
	savedIDRevision         uint64
	lastPersistedGeneration uint64
	programmaticSavedID     bool
	discoveryGeneration     uint64
	attachGeneration        uint64
	recoveryOriginByOption  map[string]string
}

func newWorkerPanel(browser *Browser, manager workerPanelController) *workerPanel {
	if manager == nil {
		return nil
	}
	p := &workerPanel{browser: browser, manager: manager}
	p.checkout = widget.NewEntry()
	p.checkout.SetPlaceHolder("Existing checkout directory")
	p.cloneSource = widget.NewEntry()
	p.cloneSource.SetPlaceHolder("Repository URL or local Git path")
	p.discoveryParent = widget.NewEntry()
	p.discoveryParent.SetPlaceHolder("Projects directory")
	p.discoveryParent.SetText(browser.projectsDirectory())
	p.discoveryParent.OnChanged = browser.setProjectsDirectory

	p.discover = widget.NewButton("Discover local checkouts", func() {
		p.discoverCheckouts(p.discoveryParent.Text)
	})
	p.checkouts = widget.NewSelect(nil, p.selectDiscoveredCheckout)
	p.checkouts.PlaceHolder = "Select a discovered checkout"
	p.checkouts.Disable()
	p.recoveryOrigins = widget.NewSelect(nil, p.selectRecoveryOrigin)
	p.recoveryOrigins.PlaceHolder = "Select a reported machine origin"
	p.recoveryOrigins.Disable()
	p.attachCheckout = widget.NewButton("Attach checkout to workspace", p.attachCheckoutToWorkspace)
	p.cloneCheckout = widget.NewButton("Clone checkout", p.cloneCheckoutForWorkspace)
	p.automaticStatus = widget.NewLabel("")
	p.automaticStatus.Wrapping = fyne.TextWrapWord
	p.discoveryStatus = widget.NewLabel("")
	p.discoveryStatus.Wrapping = fyne.TextWrapWord
	p.provider = widget.NewSelect([]string{"antigravity", "claude", "codex"}, nil)
	p.provider.SetSelected("antigravity")
	p.launchSummary = widget.NewLabel("")
	p.launchSummary.Wrapping = fyne.TextWrapWord
	p.provider.OnChanged = func(string) { p.refreshLaunchSummary() }
	p.slots = widget.NewSelect(workerSlotOptions(), nil)
	p.slots.SetSelected("1")
	p.goal = widget.NewEntry()
	p.goal.SetPlaceHolder("Optional task goal")
	p.savedID = widget.NewEntry()
	p.savedID.SetPlaceHolder("Saved worker pool ID")
	p.savedID.SetText(browser.prefs.WorkerPools[browser.prefKey])
	p.savedID.OnChanged = func(string) {
		if !p.programmaticSavedID {
			p.savedIDRevision++
		}
	}
	p.start = widget.NewButton("Start pool", p.startPool)
	p.resume = widget.NewButton("Resume saved pool", p.resumePool)
	p.stop = widget.NewButton("Stop pool", p.stopPool)
	p.status = widget.NewLabel("Worker manager is not started.")
	p.status.Wrapping = fyne.TextWrapWord
	p.identity = widget.NewLabel("")
	p.identity.Wrapping = fyne.TextWrapWord
	p.stop.Disable()
	content := container.NewVBox(
		widget.NewForm(
			widget.NewFormItem("Checkout", p.checkout),
			widget.NewFormItem("Repository source", p.cloneSource),
			widget.NewFormItem("Projects directory", container.NewBorder(nil, nil, nil, container.NewHBox(p.discover, p.cloneCheckout), p.discoveryParent)),
			widget.NewFormItem("Discovered", p.checkouts),
			widget.NewFormItem("Recovery source", p.recoveryOrigins),
			widget.NewFormItem("Workspace checkout", p.attachCheckout),
			widget.NewFormItem("Provider", p.provider),
			widget.NewFormItem("Launch preferences", p.launchSummary),
			widget.NewFormItem("Slots", p.slots),
			widget.NewFormItem("Goal", p.goal),
		),
		container.NewHBox(p.start, p.resume, p.stop),
		p.savedID,
		p.identity,
		p.discoveryStatus,
		p.status,
	)
	p.accordion = widget.NewAccordion(widget.NewAccordionItem("Native worker pool", container.NewVBox(p.automaticStatus, content)))
	p.renderSnapshot(manager.Snapshot())
	p.refreshLaunchSummary()
	p.updateControls()
	return p
}

func (p *workerPanel) restoreWorkspaceCheckout() {
	if p.browser == nil || p.checkout == nil {
		return
	}
	p.checkout.SetText(p.browser.selection.CheckoutPath)
	if p.browser.selection.CheckoutPath == "" {
		p.discoveryStatus.SetText("No checkout attached to this workspace.")
	} else {
		p.discoveryStatus.SetText("Workspace checkout attached: " + p.browser.selection.CheckoutPath)
	}
	p.discoveryGeneration++
	p.attachGeneration++
	p.refreshRecoverySources()
	p.refreshLaunchSummary()
}

func (p *workerPanel) refreshLaunchSummary() {
	if p == nil || p.browser == nil || p.launchSummary == nil || p.provider == nil {
		return
	}
	prefs := p.browser.conversationLaunchPreferences(p.provider.Selected)
	if prefs.Model == "" && prefs.Persona == "" {
		p.launchSummary.SetText("Provider defaults")
		return
	}
	parts := make([]string, 0, 2)
	if prefs.Model != "" {
		parts = append(parts, "model: "+prefs.Model)
	}
	if prefs.Persona != "" {
		parts = append(parts, "persona: "+prefs.Persona)
	}
	p.launchSummary.SetText(strings.Join(parts, " · "))
}

func (p *workerPanel) refreshRecoverySources() {
	if p == nil || p.browser == nil || p.recoveryOrigins == nil {
		return
	}
	defer func() {
		if p.root != nil {
			p.root.Refresh()
		}
	}()
	p.recoveryOriginByOption = make(map[string]string)
	p.recoveryOrigins.Options = nil
	p.recoveryOrigins.SetSelected("")
	path := p.browser.selection.CheckoutPath
	if path == "" || localCheckoutAvailable(path) {
		p.recoveryOrigins.Disable()
		return
	}
	project := p.browser.selection.Project
	var selected *storage.ProjectOverview
	for i := range p.browser.projectItems {
		if p.browser.projectItems[i].Ident == project {
			selected = &p.browser.projectItems[i]
			break
		}
	}
	if selected == nil || selected.ArchivedAt != nil {
		p.recoveryOrigins.Disable()
		return
	}
	var machineCheckout *storage.CheckoutOverview
	for i := range p.browser.checkoutItems {
		if p.browser.checkoutItems[i].Ident == project {
			machineCheckout = &p.browser.checkoutItems[i]
			break
		}
	}
	if machineCheckout == nil {
		p.recoveryOrigins.Disable()
		p.discoveryStatus.SetText("Checkout is missing; no machine-reported source is available.")
		return
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		p.recoveryOrigins.Disable()
		p.discoveryStatus.SetText("Checkout is missing; this machine could not verify its own checkout report.")
		return
	}
	seen := make(map[string]bool)
	for _, machine := range machineCheckout.Machines {
		if strings.EqualFold(strings.TrimSpace(machine.Hostname), strings.TrimSpace(hostname)) {
			continue
		}
		if machine.Origin == nil || *machine.Origin == "[redacted]" || projectid.ValidateCloneSource(*machine.Origin) != nil {
			continue
		}
		if selected.CanonicalRemote != nil {
			got, err := projectid.CanonicalRemote(*machine.Origin)
			want, wantErr := projectid.CanonicalRemote(*selected.CanonicalRemote)
			if err != nil || wantErr != nil || got != want {
				continue
			}
		} else {
			if selected.Kind != "adhoc" {
				continue
			}
			if _, err := projectid.CanonicalRemote(*machine.Origin); err == nil {
				continue
			}
		}
		label := strings.TrimSpace(machine.Hostname) + " · " + *machine.Origin
		if seen[*machine.Origin] {
			continue
		}
		seen[*machine.Origin] = true
		p.recoveryOriginByOption[label] = *machine.Origin
		p.recoveryOrigins.Options = append(p.recoveryOrigins.Options, label)
	}
	if len(p.recoveryOrigins.Options) == 0 {
		p.recoveryOrigins.Disable()
		p.discoveryStatus.SetText("Checkout is missing; no matching machine-reported source is available.")
		return
	}
	p.recoveryOrigins.Enable()
}

func (p *workerPanel) selectRecoveryOrigin(label string) {
	if origin := p.recoveryOriginByOption[label]; origin != "" {
		p.cloneSource.SetText(origin)
		p.discoveryStatus.SetText("Machine-reported source selected. Choose Clone checkout to recover this workspace.")
	}
}

func localCheckoutAvailable(directory string) bool {
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(directory, ".git"))
	return err == nil
}

func (p *workerPanel) attachCheckoutToWorkspace() {
	b := p.browser
	if p.closed || b == nil || b.activeWorkspaceID == "" || b.selection.Project == "" {
		p.discoveryStatus.SetText("Select a project in this workspace before attaching a checkout.")
		return
	}
	workspaceID, project := b.activeWorkspaceID, b.selection.Project
	revision := b.workspaceBindingGeneration[workspaceID]
	attachGeneration := p.attachGeneration + 1
	p.attachGeneration = attachGeneration
	directory := strings.TrimSpace(p.checkout.Text)
	if directory == "" {
		p.discoveryStatus.SetText("Choose an existing checkout before attaching it.")
		return
	}
	p.attachCheckout.Disable()
	p.cloneCheckout.Disable()
	p.discoveryStatus.SetText("Verifying checkout and project binding…")
	if !b.launch(func(ctx context.Context) {
		identity, err := projectid.ResolveCheckout(ctx, directory)
		var origin *string
		if err == nil {
			origin, err = projectid.CloneOrigin(ctx, identity.Directory)
		}
		b.dispatch(func() {
			p.finishCheckoutBinding(attachGeneration, workspaceID, revision, project, identity, origin, err, "attached")
		})
	}) {
		p.attachCheckout.Enable()
		p.cloneCheckout.Enable()
		p.discoveryStatus.SetText("Checkout verification could not start.")
	}
}

func (p *workerPanel) cloneCheckoutForWorkspace() {
	b := p.browser
	if p.closed || b == nil || b.activeWorkspaceID == "" || b.selection.Project == "" {
		p.discoveryStatus.SetText("Select a project in this workspace before cloning a checkout.")
		return
	}
	parent, source := strings.TrimSpace(p.discoveryParent.Text), strings.TrimSpace(p.cloneSource.Text)
	if parent == "" || source == "" {
		p.discoveryStatus.SetText("Enter a repository source and projects directory, then explicitly choose Clone checkout.")
		return
	}
	workspaceID, project := b.activeWorkspaceID, b.selection.Project
	revision := b.workspaceBindingGeneration[workspaceID]
	p.attachGeneration++
	operation := p.attachGeneration
	p.attachCheckout.Disable()
	p.cloneCheckout.Disable()
	p.discoveryStatus.SetText("Cloning the entered repository source…")
	if !b.launch(func(ctx context.Context) {
		directory, err := projectid.CloneRepository(ctx, parent, source)
		var identity projectid.Identity
		var origin *string
		if err == nil {
			identity, err = projectid.ResolveCheckout(ctx, directory)
		}
		if err == nil {
			origin, err = projectid.CloneOrigin(ctx, identity.Directory)
		}
		b.dispatch(func() {
			p.finishCheckoutBinding(operation, workspaceID, revision, project, identity, origin, err, "cloned")
		})
	}) {
		p.attachCheckout.Enable()
		p.cloneCheckout.Enable()
		p.discoveryStatus.SetText("Clone could not start.")
	}
}

func (p *workerPanel) finishCheckoutBinding(operation uint64, workspaceID string, revision uint64, project string, identity projectid.Identity, origin *string, err error, action string) {
	if p.closed || p.attachGeneration != operation {
		return
	}
	p.attachCheckout.Enable()
	p.cloneCheckout.Enable()
	if err != nil {
		p.discoveryStatus.SetText("Checkout was not bound: " + err.Error())
		return
	}
	b := p.browser
	if b.activeWorkspaceID != workspaceID || b.workspaceBindingGeneration[workspaceID] != revision || b.selection.Project != project {
		p.discoveryStatus.SetText("Workspace or project changed while the checkout was being verified.")
		return
	}
	var currentProject *storage.ProjectOverview
	for i := range b.projectItems {
		if b.projectItems[i].Ident == project {
			currentProject = &b.projectItems[i]
			break
		}
	}
	if currentProject == nil || currentProject.ArchivedAt != nil {
		p.discoveryStatus.SetText("Checkout was not bound: selected project is unavailable.")
		return
	}
	if err := validateWorkspaceCheckoutProject(*currentProject, identity); err != nil {
		p.discoveryStatus.SetText("Checkout was not bound: " + err.Error())
		return
	}
	if !b.bindWorkspaceCheckoutWithOrigin(workspaceID, revision, project, identity.Directory, origin) {
		p.discoveryStatus.SetText("Workspace or project changed while the checkout was being verified.")
		return
	}
	p.checkout.SetText(identity.Directory)
	p.refreshRecoverySources()
	if b.client != nil && b.pluginSettings != nil {
		b.Refresh()
	}
	if action == "cloned" {
		p.discoveryStatus.SetText("Clone completed and checkout attached to this workspace. No worker was started.")
	} else {
		p.discoveryStatus.SetText("Existing checkout attached to this workspace. No worker was started.")
	}
}

func validateWorkspaceCheckoutProject(item storage.ProjectOverview, identity projectid.Identity) error {
	return projectid.ValidateBinding(item.Kind, item.CanonicalRemote, identity)
}

func (p *workerPanel) discoverCheckouts(parent string) {
	p.discoveryGeneration++
	generation := p.discoveryGeneration
	p.checkouts.Options = nil
	p.checkouts.SetSelected("")
	p.checkouts.Disable()
	p.discoveryStatus.SetText("Looking for immediate Git checkouts…")
	if !p.browser.launch(func(ctx context.Context) {
		found := projectid.DiscoverLocalCheckouts(ctx, parent)
		paths := make([]string, 0, len(found))
		for _, checkout := range found {
			paths = append(paths, checkout.Directory)
		}
		p.browser.dispatch(func() { p.finishCheckoutDiscovery(generation, paths) })
	}) {
		p.finishCheckoutDiscovery(generation, nil)
	}
}

func (p *workerPanel) finishCheckoutDiscovery(generation uint64, paths []string) {
	if p.closed || p.discoveryGeneration != generation {
		return
	}
	p.checkouts.Options = paths
	if len(paths) == 0 {
		p.discoveryStatus.SetText("No immediate Git checkouts found in that directory.")
		p.checkouts.Disable()
	} else {
		p.discoveryStatus.SetText(fmt.Sprintf("Found %d immediate checkout(s). Selecting one only fills the checkout field.", len(paths)))
		p.checkouts.Enable()
	}
	if p.root != nil {
		p.root.Refresh()
	}
}

func (p *workerPanel) selectDiscoveredCheckout(directory string) {
	if directory == "" {
		return
	}
	p.checkout.SetText(directory)
	p.discoveryStatus.SetText("Existing checkout selected. Starting a worker pool is a separate action.")
}

func workerSlotOptions() []string {
	values := make([]string, 32)
	for i := range values {
		values[i] = strconv.Itoa(i + 1)
	}
	return values
}

func workerPoolOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func (p *workerPanel) lifetimeStarted(ctx context.Context, err error) {
	p.browser.mu.Lock()
	closing := p.browser.closing
	p.browser.mu.Unlock()
	if closing || ctx.Err() != nil {
		return
	}
	if err != nil {
		p.startError = err.Error()
		p.status.SetText("Worker manager could not start: " + err.Error())
		p.updateControls()
		return
	}
	p.started = true
	p.lastPersistedGeneration = p.manager.Snapshot().Generation
	p.renderSnapshot(p.manager.Snapshot())
	p.updateControls()
	p.watch(ctx)
}

func (p *workerPanel) watch(ctx context.Context) {
	b := p.browser
	b.mu.Lock()
	if b.closing || ctx.Err() != nil {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.dispatch(func() {
					b.mu.Lock()
					closing := b.closing
					b.mu.Unlock()
					if !p.closed && !closing {
						p.renderSnapshot(p.manager.Snapshot())
						p.updateControls()
					}
				})
			}
		}
	}()
}

func (p *workerPanel) startPool() {
	slots, err := strconv.Atoi(p.slots.Selected)
	if err != nil || slots < 1 || slots > 32 || strings.TrimSpace(p.checkout.Text) == "" || p.provider.Selected == "" {
		p.status.SetText("Choose an existing checkout, provider, and 1 to 32 slots.")
		return
	}
	p.browser.mu.Lock()
	project := p.browser.selectedProject
	server := p.browser.endpoint
	p.browser.mu.Unlock()
	if project == "" {
		p.status.SetText("Select a project before starting a worker pool.")
		return
	}
	directory, err := process.ValidateDirectory(p.checkout.Text, p.provider.Selected)
	if err != nil {
		p.status.SetText(err.Error())
		return
	}
	command, err := providerLaunchCommand(p.provider.Selected, p.browser.conversationLaunchPreferences(p.provider.Selected))
	if err != nil {
		p.status.SetText(err.Error())
		return
	}
	pool := storage.WorkerPool{
		ID:        uuid.NewString(),
		Server:    server,
		Project:   project,
		Directory: directory,
		Provider:  p.provider.Selected,
		Command:   command,
		OS:        workerPoolOS(),
		SlotCount: slots,
	}
	if goal := strings.TrimSpace(p.goal.Text); goal != "" {
		pool.Goal = &goal
	}
	savedIDRevision := p.savedIDRevision
	p.run("Starting worker pool…", savedIDRevision, func() (*storage.WorkerPool, error) {
		return p.manager.CreateAndStartPool(pool)
	})
}

func (p *workerPanel) resumePool() {
	poolID := strings.TrimSpace(p.savedID.Text)
	if poolID == "" {
		p.status.SetText("Enter a saved worker pool ID to resume it.")
		return
	}
	savedIDRevision := p.savedIDRevision
	p.run("Resuming saved worker pool…", savedIDRevision, func() (*storage.WorkerPool, error) {
		err := p.manager.ResumePool(poolID)
		snapshot := p.manager.Snapshot()
		if err == nil && snapshot.Pool != nil && snapshot.Pool.ID == poolID {
			return snapshot.Pool, err
		}
		return nil, err
	})
}

func (p *workerPanel) run(message string, savedIDRevision uint64, operation func() (*storage.WorkerPool, error)) {
	b := p.browser
	b.mu.Lock()
	if b.closing || !b.started || b.ctx == nil || p.closed || p.busy {
		b.mu.Unlock()
		return
	}
	p.busy = true
	p.requestError = ""
	p.op++
	op := p.op
	b.wg.Add(1)
	p.status.SetText(message)
	p.updateControlsLocked()
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		pool, err := operation()
		snapshot := p.manager.Snapshot()
		b.dispatch(func() { p.finishAction(op, savedIDRevision, snapshot, pool, err) })
	}()
}

func (p *workerPanel) finishAction(op, savedIDRevision uint64, snapshot WorkerManagerSnapshot, pool *storage.WorkerPool, err error) {
	b := p.browser
	b.mu.Lock()
	if b.closing || p.closed || p.op != op {
		b.mu.Unlock()
		return
	}
	p.busy = false
	if err != nil {
		p.requestError = err.Error()
	}
	if pool != nil && strings.TrimSpace(pool.ID) != "" {
		p.persistPoolID(snapshot, pool.ID)
		if p.savedIDRevision == savedIDRevision {
			p.programmaticSavedID = true
			p.savedID.SetText(pool.ID)
			p.programmaticSavedID = false
		}
	}
	p.renderSnapshot(snapshot)
	p.updateControlsLocked()
	b.mu.Unlock()
}

func (p *workerPanel) persistPoolID(snapshot WorkerManagerSnapshot, poolID string) {
	if snapshot.Generation == 0 || snapshot.Generation < p.lastPersistedGeneration {
		return
	}
	if snapshot.Generation == p.lastPersistedGeneration && p.browser.prefs.WorkerPools[p.browser.prefKey] == poolID {
		return
	}
	if p.browser.prefs.WorkerPools == nil {
		p.browser.prefs.WorkerPools = make(map[string]string)
	}
	p.browser.prefs.WorkerPools[p.browser.prefKey] = poolID
	p.lastPersistedGeneration = snapshot.Generation
	p.browser.persist()
}

func (p *workerPanel) renderSnapshot(snapshot WorkerManagerSnapshot) {
	oldStatus, oldIdentity := p.status.Text, p.identity.Text
	if p.startError != "" {
		p.status.SetText("Worker manager could not start: " + p.startError)
	} else if p.requestError != "" {
		p.status.SetText("Worker pool action failed: " + p.requestError)
	} else {
		switch snapshot.State {
		case WorkerManagerIdle:
			p.status.SetText("Worker pool is stopped.")
		case WorkerManagerStarting:
			p.status.SetText("Starting worker pool…")
		case WorkerManagerRunning:
			p.status.SetText("Worker pool is running.")
		case WorkerManagerStopping:
			p.status.SetText("Stopping worker pool…")
		case WorkerManagerStopped:
			p.status.SetText("Worker pool is stopped.")
		case WorkerManagerFailed:
			p.status.SetText("Worker pool failed: " + snapshot.Error)
		default:
			p.status.SetText("Worker pool status is unavailable.")
		}
	}
	if snapshot.Pool == nil {
		p.identity.SetText("")
	} else {
		p.identity.SetText(workerPoolIdentity(*snapshot.Pool))
	}
	if p.busy || p.closed {
		p.start.Disable()
		p.resume.Disable()
		p.stop.Disable()
	}
	if p.root != nil && (p.status.Text != oldStatus || p.identity.Text != oldIdentity) {
		p.root.Refresh()
	}
}

func (p *workerPanel) setRoot(root *container.Split) {
	p.root = root
}

func (p *workerPanel) updateControls() {
	p.browser.mu.Lock()
	p.updateControlsLocked()
	p.browser.mu.Unlock()
}

func (p *workerPanel) updateControlsLocked() {
	available := p.started && !p.browser.closing && p.browser.ctx != nil && p.startError == ""
	snapshot := p.manager.Snapshot()
	active := snapshot.State == WorkerManagerStarting || snapshot.State == WorkerManagerRunning || snapshot.State == WorkerManagerStopping
	if !available || p.closed || p.busy || active {
		p.start.Disable()
		p.resume.Disable()
	} else {
		p.start.Enable()
		p.resume.Enable()
	}
	if !available || p.closed || p.busy || !active || snapshot.State == WorkerManagerStopping {
		p.stop.Disable()
	} else {
		p.stop.Enable()
	}
}

func workerPoolIdentity(pool storage.WorkerPool) string {
	goal := ""
	if pool.Goal != nil {
		goal = *pool.Goal
	}
	return fmt.Sprintf("Saved pool %s\n%s · %s · %d slots\n%s\nGoal: %s", pool.ID, pool.Project, pool.Provider, pool.SlotCount, pool.Directory, goal)
}

func (p *workerPanel) stopPool() {
	p.run("Stopping worker pool…", p.savedIDRevision, func() (*storage.WorkerPool, error) {
		p.manager.Stop()
		return nil, nil
	})
}

func (p *workerPanel) close() {
	p.closed = true
	p.discoveryGeneration++
	p.start.Disable()
	p.resume.Disable()
	p.stop.Disable()
}
