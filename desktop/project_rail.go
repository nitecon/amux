package desktop

import (
	"path/filepath"

	"github.com/nitecon/amux/storage"
)

func (b *Browser) filteredProjectRail() []storage.ProjectOverview {
	rows := append([]storage.ProjectOverview(nil), b.projectItems...)
	aliases := b.projectSearchAliases()
	b.localRailProjects = make(map[string]string)
	b.discoveredRailProjects = make(map[string]bool)
	favorites := make(map[string]bool)
	for _, checkout := range b.localCheckouts {
		matched := false
		for _, project := range b.projectItems {
			if checkout.CanonicalRemote == nil {
				break
			}
			if same, _ := checkoutMatchesProjectRemote(*checkout.CanonicalRemote, project.CanonicalRemote); same {
				aliases[project.Ident] = append(aliases[project.Ident], filepath.Base(checkout.Directory), checkout.Directory)
				saved := b.prefs.ProjectCheckouts[FavoriteKey(b.prefKey, project.Ident)]
				attached := localCheckoutAvailable(saved.Directory) ||
					(b.selection.Project == project.Ident && localCheckoutAvailable(b.selection.CheckoutPath))
				b.discoveredRailProjects[project.Ident] = !attached
				matched = true
			}
		}
		if matched {
			continue
		}
		key := "local:" + checkout.Directory
		b.localRailProjects[key] = checkout.Directory
		aliases[key] = []string{filepath.Base(checkout.Directory), checkout.Directory}
		rows = append(rows, storage.ProjectOverview{Project: storage.Project{Ident: key}})
	}
	if b.localRailSelection != "" && b.localRailProjects[b.localRailSelection] == "" {
		b.localRailSelection = ""
		b.syncWorkspaceFiles()
	}
	for _, row := range rows {
		if b.railProjectFavorite(row.Ident) {
			favorites[FavoriteKey(b.endpoint, row.Ident)] = true
		}
	}
	filtered := filterProjects(rows, favorites, b.endpoint, b.selection.ProjectFilter, aliases)
	grouped := make([]storage.ProjectOverview, 0, len(filtered))
	for _, group := range []string{"FAVORITES", "LOCAL PROJECTS", "GATEWAY PROJECTS"} {
		for _, row := range filtered {
			if b.railProjectGroup(row.Ident) == group {
				grouped = append(grouped, row)
			}
		}
	}
	return grouped
}

func (b *Browser) railProjectFavorite(id string) bool {
	if dir := b.localRailProjects[id]; dir != "" {
		return b.prefs.Favorites["local:"+filepath.Clean(dir)]
	}
	return id != "" && (projectRemoteFavorite(b.prefs.Favorites, b.endpoint, b.clusterID, id) ||
		(id == b.selectedProject && b.projectFavoriteIsSet(id)))
}

func (b *Browser) railProjectGroup(id string) string {
	if b.railProjectFavorite(id) {
		return "FAVORITES"
	}
	if b.localRailProjects[id] != "" || b.discoveredRailProjects[id] {
		return "LOCAL PROJECTS"
	}
	return "GATEWAY PROJECTS"
}

func (b *Browser) projectGroupAt(index int) string {
	group := b.railProjectGroup(b.visibleProjects[index].Ident)
	if index == 0 || group != b.railProjectGroup(b.visibleProjects[index-1].Ident) {
		return group
	}
	return ""
}

func (b *Browser) selectedRailProject() string {
	if b.localRailSelection != "" {
		return b.localRailSelection
	}
	return b.selectedProject
}

func (b *Browser) selectRailProject(id string) {
	if id == b.selectedRailProject() {
		if b.localRailSelection != "" && b.mainView != "Files" {
			b.setMainView("Files")
		}
		return
	}
	if b.localRailProjects[id] == "" {
		b.selectProject(id)
		return
	}
	b.selectProject("")
	b.localRailSelection = id
	b.syncProjectSelection()
	b.projects.Refresh()
	b.syncWorkspaceFiles()
	b.updateSelectedProjectStatus()
	b.setMainView("Files")
}

func (b *Browser) updateSelectedProjectStatus() {
	if b.selectedProjectStatus == nil {
		return
	}
	project, path := b.selectedProject, b.selection.CheckoutPath
	if directory := b.localRailProjects[b.localRailSelection]; directory != "" {
		project, path = filepath.Base(directory), directory
	} else if path == "" {
		for _, row := range b.projectItems {
			if row.Ident == project {
				path = b.projectCheckout(row).Directory
				break
			}
		}
	}
	if project == "" {
		b.selectedProjectStatus.SetText("Selected Project: —")
		return
	}
	if path == "" {
		path = "No local checkout"
	}
	b.selectedProjectStatus.SetText("Selected Project: " + project + "  |  Path: " + path)
}
