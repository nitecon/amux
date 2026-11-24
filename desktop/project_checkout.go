package desktop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
)

type projectMenuRow struct {
	widget.BaseWidget
	content  *fyne.Container
	onMenu   func(*fyne.PointEvent)
	onSelect func()
}

func newProjectMenuRow(content *fyne.Container) *projectMenuRow {
	row := &projectMenuRow{content: content}
	row.ExtendBaseWidget(row)
	return row
}

func (r *projectMenuRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(r.content)
}

func (r *projectMenuRow) Tapped(*fyne.PointEvent) {
	if r.onSelect != nil {
		r.onSelect()
	}
}

func (r *projectMenuRow) TappedSecondary(event *fyne.PointEvent) {
	if r.onMenu != nil {
		r.onMenu(event)
	}
}

func (b *Browser) showProjectMenu(row *projectMenuRow, event *fyne.PointEvent, project storage.ProjectOverview) {
	canvas := fyne.CurrentApp().Driver().CanvasForObject(row)
	if canvas == nil {
		return
	}
	var popup *widget.PopUp
	action := func(label string, icon fyne.Resource, run func()) *widget.Button {
		button := widget.NewButtonWithIcon(label, icon, func() {
			popup.Hide()
			run()
		})
		button.Alignment = widget.ButtonAlignLeading
		button.Importance = widget.LowImportance
		return button
	}
	checkout := action("Check out…", theme.FolderOpenIcon(), func() { b.openProjectCheckout(project) })
	if b.localRailProjects[project.Ident] != "" {
		checkout = action("Open local files", theme.FolderOpenIcon(), func() { b.selectRailProject(project.Ident) })
	}
	options := container.NewVBox(
		checkout,
		action("☆  "+b.favoriteText(project.Ident), nil, func() { b.toggleFavoriteFor(project.Ident) }),
	)
	surface := browserCardSurface()
	surface.StrokeColor = theme.PrimaryColor()
	surface.SetMinSize(fyne.NewSize(144, 0))
	content := container.NewStack(surface, container.NewPadded(options))
	popup = widget.NewPopUp(container.NewThemeOverride(content, toolbarTheme{Theme: fyne.CurrentApp().Settings().Theme()}), canvas)
	position := event.AbsolutePosition
	size := popup.MinSize()
	position.X = max(0, min(position.X, canvas.Size().Width-size.Width))
	position.Y = max(0, min(position.Y, canvas.Size().Height-size.Height))
	popup.ShowAtPosition(position)
}

type localProjectBinding struct {
	Directory string  `json:"directory"`
	Origin    *string `json:"origin,omitempty"`
}

func (b *Browser) projectsDirectory() string {
	if b.prefs.ProjectsDirectory != "" {
		return b.prefs.ProjectsDirectory
	}
	return b.prefs.ProjectsDirectories[b.prefKey]
}

func (b *Browser) saveProjectsDirectory(value string) error {
	value = strings.TrimSpace(value)
	if value != "" {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("enter an absolute projects directory")
		}
		info, err := os.Stat(value)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("choose an existing projects directory")
		}
		value = filepath.Clean(value)
	}
	if value == b.prefs.ProjectsDirectory && len(b.prefs.ProjectsDirectories) == 0 {
		b.scanProjectCheckouts()
	} else {
		b.setProjectsDirectory(value)
	}
	return nil
}

func (b *Browser) setProjectsDirectory(value string) {
	if b.prefs.ProjectsDirectory == value && len(b.prefs.ProjectsDirectories) == 0 {
		return
	}
	b.prefs.ProjectsDirectory = value
	b.prefs.ProjectsDirectories = nil
	if b.projectsDirectoryEntry != nil {
		b.projectsDirectoryEntry.SetText(value)
	}
	if b.workerPanel != nil && b.workerPanel.discoveryParent.Text != value {
		b.workerPanel.discoveryParent.SetText(value)
	}
	b.persist()
	b.scanProjectCheckouts()
}

func (b *Browser) scanProjectCheckouts() {
	b.checkoutScan++
	generation := b.checkoutScan
	parent := b.projectsDirectory()
	if parent == "" {
		b.showLocalCheckouts(nil)
		return
	}
	b.launch(func(ctx context.Context) {
		found := projectidentity.DiscoverLocalCheckouts(ctx, parent)
		b.dispatch(func() {
			b.mu.Lock()
			closing := b.closing
			b.mu.Unlock()
			if closing || b.checkoutScan != generation {
				return
			}
			b.showLocalCheckouts(found)
		})
	})
}

func (b *Browser) showLocalCheckouts(found []projectidentity.LocalCheckout) {
	b.localCheckouts = found
	if b.localCheckoutSummary != nil {
		switch {
		case b.projectsDirectory() == "":
			b.localCheckoutSummary.SetText("")
		case len(found) == 0:
			b.localCheckoutSummary.SetText("No Git checkouts found in this directory.")
		default:
			names := make([]string, 0, len(found))
			for _, checkout := range found {
				names = append(names, filepath.Base(checkout.Directory))
			}
			b.localCheckoutSummary.SetText("Local checkouts: " + strings.Join(names, ", "))
		}
	}
	if b.projects != nil {
		b.applyProjectFilter()
		b.updateSelectedProjectStatus()
		b.updateAutomaticWorkerScopes()
	}
}

func (b *Browser) projectCheckout(project storage.ProjectOverview) localProjectBinding {
	binding := b.prefs.ProjectCheckouts[FavoriteKey(b.prefKey, project.Ident)]
	if binding.Directory != "" && localCheckoutAvailable(binding.Directory) {
		return binding
	}
	for _, checkout := range b.localCheckouts {
		if checkout.CanonicalRemote == nil || project.CanonicalRemote == nil {
			continue
		}
		if matches, _ := checkoutMatchesProjectRemote(*checkout.CanonicalRemote, project.CanonicalRemote); matches {
			return localProjectBinding{Directory: checkout.Directory}
		}
	}
	if b.selection.Project == project.Ident && localCheckoutAvailable(b.selection.CheckoutPath) {
		return localProjectBinding{Directory: b.selection.CheckoutPath, Origin: b.selection.CheckoutOrigin}
	}
	return localProjectBinding{}
}

func (b *Browser) restoreProjectCheckout(project string) {
	binding := b.prefs.ProjectCheckouts[FavoriteKey(b.prefKey, project)]
	if binding.Directory != "" && localCheckoutAvailable(binding.Directory) {
		b.selection.CheckoutPath = binding.Directory
		b.selection.CheckoutOrigin = binding.Origin
		if b.workerPanel != nil {
			b.workerPanel.restoreWorkspaceCheckout()
		}
	}
}

func (b *Browser) openProjectCheckout(project storage.ProjectOverview) {
	if b.activeWorkspaceID == "" {
		b.browserStatus.SetText("Open a workspace before checking out a project.")
		return
	}
	if b.selectedProject != project.Ident {
		b.selectProject(project.Ident)
	}
	previous := b.selection.MainView
	if previous == "" {
		previous = "Tasks"
	}
	back := widget.NewButtonWithIcon("Back", theme.NavigateBackIcon(), func() { b.setMainView(previous) })
	parent := b.projectsDirectory()
	local := b.projectCheckout(project)
	source := widget.NewEntry()
	source.SetPlaceHolder("Git repository URL")
	if local.Origin != nil {
		source.SetText(*local.Origin)
	} else if project.CanonicalRemote != nil {
		remote := *project.CanonicalRemote
		if !strings.Contains(remote, "://") && !strings.Contains(remote, "@") {
			remote = "https://" + remote
		}
		source.SetText(remote)
	}
	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord
	progress := widget.NewProgressBarInfinite()
	progress.Hide()
	progress.Stop()
	var page fyne.CanvasObject
	var options []*widget.Button
	setBusy := func(busy bool) {
		for _, button := range options {
			if busy {
				button.Disable()
			} else {
				button.Enable()
			}
		}
		if busy {
			source.Disable()
		} else {
			source.Enable()
			if parent == "" {
				options[1].Disable()
			}
			progress.Stop()
			progress.Hide()
		}
	}
	start := func(method string) {
		if b.selection.Project != project.Ident {
			status.SetText("Select this project again to check it out in the active workspace.")
			return
		}
		workspace := b.activeWorkspaceID
		revision := b.workspaceBindingGeneration[workspace]
		repository := strings.TrimSpace(source.Text)
		var destination string
		if method == cloneInProjects {
			var err error
			destination, err = projectidentity.CloneDestination(parent, repository)
			if err != nil {
				status.SetText(err.Error())
				return
			}
		} else if method == cloneAtLocation && repository == "" {
			status.SetText("Enter the repository URL to clone.")
			return
		}
		setBusy(true)
		status.SetText("")
		showProgress := func(folder string) {
			if method == openInFolder {
				status.SetText("Verifying checkout at " + folder)
			} else {
				status.SetText("Cloning into " + folder)
			}
			progress.Show()
			progress.Start()
		}
		if destination != "" {
			showProgress(destination)
		}
		if !b.launchWithTimeout(0, func(ctx context.Context) {
			if method != cloneInProjects {
				title, initial := "Choose the existing checkout", local.Directory
				if method == cloneAtLocation {
					title, initial = "Choose an empty folder for the clone", parent
				}
				var err error
				destination, err = pickCheckoutFolder(ctx, title, initial)
				if err != nil || destination == "" {
					b.dispatch(func() {
						if ctx.Err() == nil {
							setBusy(false)
							if err != nil {
								status.SetText(err.Error())
							}
						}
					})
					return
				}
				b.dispatch(func() {
					if ctx.Err() == nil {
						showProgress(destination)
					}
				})
			}
			identity, origin, err := prepareProjectCheckout(ctx, project, method, parent, destination, repository)
			b.dispatch(func() {
				if ctx.Err() != nil {
					return
				}
				setBusy(false)
				if err != nil {
					status.SetText(err.Error())
					return
				}
				if b.checkoutPage.Objects[0] != page || !b.bindWorkspaceCheckoutWithOrigin(workspace, revision, project.Ident, identity.Directory, origin) {
					status.SetText("The active workspace or project changed. Open checkout again to attach this folder.")
					return
				}
				if b.workerPanel != nil {
					b.workerPanel.restoreWorkspaceCheckout()
				}
				b.scanProjectCheckouts()
				b.applyProjectFilter()
				b.Refresh()
				status.SetText("Checkout ready at " + identity.Directory)
			})
		}) {
			setBusy(false)
			status.SetText("The desktop is closing.")
		}
	}
	choices := container.New(layout.NewCustomPaddedVBoxLayout(8))
	for _, option := range []struct{ name, description string }{
		{openInFolder, "Use a repository that is already on this machine."},
		{cloneInProjects, parent},
		{cloneAtLocation, "Choose the folder where Git should put the repository."},
	} {
		description := option.description
		if description == "" {
			description = "Set the projects directory in Settings → Workspace."
		}
		button := widget.NewButtonWithIcon(option.name, theme.FolderOpenIcon(), func() { start(option.name) })
		button.Alignment = widget.ButtonAlignLeading
		options = append(options, button)
		choices.Add(settingsCard(container.NewVBox(button, widget.NewLabel(description))))
	}
	setBusy(false)
	form := container.New(layout.NewCustomPaddedVBoxLayout(10),
		widget.NewLabel(project.Ident), choices, widget.NewLabel("Repository URL for cloning"), source,
		status, progress, container.NewHBox(back, layout.NewSpacer()))
	controls := toolbarTheme{Theme: fyne.CurrentApp().Settings().Theme()}
	page = browserPanel(settingsPage("Check out project", container.NewThemeOverride(form, controls)))
	b.checkoutPage.Objects = []fyne.CanvasObject{page}
	b.checkoutPage.Refresh()
	b.setMainView("Checkout")
}

const (
	openInFolder    = "Open in Folder"
	cloneInProjects = "Clone in Projects Directory"
	cloneAtLocation = "Clone at Location"
)

func prepareProjectCheckout(ctx context.Context, project storage.ProjectOverview, method, parent, location, source string) (projectidentity.Identity, *string, error) {
	if method != openInFolder && project.CanonicalRemote != nil {
		matches, _ := checkoutMatchesProjectRemote(source, project.CanonicalRemote)
		if !matches {
			return projectidentity.Identity{}, nil, fmt.Errorf("repository URL does not match the selected project")
		}
	}
	var err error
	switch method {
	case cloneInProjects:
		location, err = projectidentity.CloneRepository(ctx, parent, source)
	case cloneAtLocation:
		location, err = projectidentity.CloneRepositoryAt(ctx, location, source)
	case openInFolder:
	default:
		return projectidentity.Identity{}, nil, fmt.Errorf("choose a checkout option")
	}
	if err != nil {
		return projectidentity.Identity{}, nil, err
	}
	identity, err := projectidentity.ResolveCheckout(ctx, location)
	if err != nil {
		return identity, nil, err
	}
	if err := validateWorkspaceCheckoutProject(project, identity); err != nil {
		return identity, nil, err
	}
	origin, err := projectidentity.CloneOrigin(ctx, identity.Directory)
	return identity, origin, err
}
