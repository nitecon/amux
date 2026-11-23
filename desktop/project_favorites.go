package desktop

import (
	"encoding/base64"
	"path/filepath"
	"strings"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

func projectFavoriteKeys(endpoint, checkoutPath, project string, clusterIDs ...string) []string {
	keys := make([]string, 0, 2)
	if path := strings.TrimSpace(checkoutPath); path != "" {
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		keys = append(keys, "local:"+filepath.Clean(path))
	}
	if strings.TrimSpace(project) != "" {
		clusterID := ""
		if len(clusterIDs) != 0 {
			clusterID = strings.TrimSpace(clusterIDs[0])
		}
		if clusterID != "" {
			keys = append(keys, clusterFavoriteKey(clusterID, project))
		} else {
			keys = append(keys, FavoriteKey(endpoint, project))
		}
	}
	return keys
}

func migrateEndpointFavorites(favorites map[string]bool, endpoint, clusterID string) bool {
	if strings.TrimSpace(clusterID) == "" {
		return false
	}
	encode := base64.RawURLEncoding.EncodeToString
	legacyPrefix := "remote:" + encode([]byte(endpoint)) + ":"
	changed := false
	for key, pinned := range favorites {
		if !pinned || !strings.HasPrefix(key, legacyPrefix) {
			continue
		}
		projectKey := strings.TrimPrefix(key, legacyPrefix)
		if projectKey == "" {
			continue
		}
		favorites["cluster:"+encode([]byte(clusterID))+":"+projectKey] = true
		delete(favorites, key)
		changed = true
	}
	return changed
}

func reconcileProjectFavorite(favorites map[string]bool, keys []string) bool {
	if len(keys) != 2 || !favorites[keys[0]] {
		return false
	}
	delete(favorites, keys[0])
	favorites[keys[1]] = true
	return true
}

func toggleProjectFavorite(favorites map[string]bool, keys []string) {
	pinned := false
	for _, key := range keys {
		pinned = pinned || favorites[key]
	}
	for _, key := range keys {
		delete(favorites, key)
	}
	if !pinned && len(keys) > 0 {
		favorites[keys[len(keys)-1]] = true
	}
}

func (b *Browser) activeProjectFavoriteKeys(project string) []string {
	return projectFavoriteKeys(b.endpoint, b.selection.CheckoutPath, project, b.clusterID)
}

func (b *Browser) projectFavoriteIsSet(project string) bool {
	for _, key := range b.activeProjectFavoriteKeys(project) {
		if b.prefs.Favorites[key] {
			return true
		}
	}
	return false
}

// reconcileWorkspaceProjectFavorites promotes checkout pins only after the
// gateway has returned a project whose canonical remote matches the verified
// checkout. Remote keys use a pinned cluster identity when available and
// retain endpoint scoping otherwise.
func (b *Browser) reconcileWorkspaceProjectFavorites() bool {
	changed := false
	for _, workspaceID := range b.workspaceIDs {
		selection := b.prefs.Selections[workspaceSelectionKey(b.prefKey, workspaceID)]
		if workspaceID == b.activeWorkspaceID {
			selection = b.selection
		}
		if selection.CheckoutPath == "" || selection.Project == "" {
			continue
		}
		project, ok := verifiedFavoriteProject(selection, b.projectItems)
		if !ok {
			continue
		}
		if reconcileProjectFavorite(b.prefs.Favorites, projectFavoriteKeys(b.endpoint, selection.CheckoutPath, project.Ident, b.clusterID)) {
			changed = true
		}
	}
	return changed
}

func verifiedFavoriteProject(selection NavigationSelection, items []storage.ProjectOverview) (storage.Project, bool) {
	for _, item := range items {
		if item.Ident != selection.Project || item.ArchivedAt != nil {
			continue
		}
		if item.CanonicalRemote == nil {
			return item.Project, item.Kind == "adhoc" && selection.CheckoutOrigin == nil
		}
		if selection.CheckoutOrigin == nil {
			return storage.Project{}, false
		}
		matches, err := checkoutMatchesProjectRemote(*selection.CheckoutOrigin, item.CanonicalRemote)
		if err != nil || !matches {
			return storage.Project{}, false
		}
		return item.Project, true
	}
	return storage.Project{}, false
}

func checkoutMatchesProjectRemote(checkoutRemote string, projectRemote *string) (bool, error) {
	checkout, err := projects.CanonicalRemote(checkoutRemote)
	if err != nil {
		return false, err
	}
	if projectRemote == nil {
		return false, nil
	}
	project, err := projects.CanonicalRemote(*projectRemote)
	if err != nil {
		return false, err
	}
	return checkout == project, nil
}
