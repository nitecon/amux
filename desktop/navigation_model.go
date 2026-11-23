package desktop

import (
	"encoding/base64"
	"sort"
	"strings"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

// FavoriteKey identifies a project favorite within one gateway endpoint. It is
// retained as the fallback and migration key for un-enrolled connections.
func FavoriteKey(endpoint, project string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return "remote:" + encode([]byte(endpoint)) + ":" + encode([]byte(projects.NormalizeIdent(project)))
}

func clusterFavoriteKey(clusterID, project string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return "cluster:" + encode([]byte(clusterID)) + ":" + encode([]byte(projects.NormalizeIdent(project)))
}

// FilterProjects filters and orders remote projects without changing rows.
func FilterProjects(rows []storage.ProjectOverview, favorites map[string]bool, endpoint, query string) []storage.ProjectOverview {
	return filterProjects(rows, favorites, endpoint, query, nil)
}

func filterProjects(rows []storage.ProjectOverview, favorites map[string]bool, endpoint, query string, aliases map[string][]string) []storage.ProjectOverview {
	return filterProjectsWithCluster(rows, favorites, endpoint, "", query, aliases)
}

func filterProjectsWithCluster(rows []storage.ProjectOverview, favorites map[string]bool, endpoint, clusterID, query string, aliases map[string][]string) []storage.ProjectOverview {
	query = strings.ToLower(strings.TrimSpace(query))
	out := make([]storage.ProjectOverview, 0, len(rows))
	exactExists := false
	if query != "" {
		for _, row := range rows {
			if projectMatchesSearch(row.Ident, aliases[row.Ident], query, true) {
				exactExists = true
				break
			}
		}
	}
	for _, row := range rows {
		if query != "" {
			if !projectMatchesSearch(row.Ident, aliases[row.Ident], query, exactExists) {
				continue
			}
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		aFavorite := projectRemoteFavorite(favorites, endpoint, clusterID, a.Ident)
		bFavorite := projectRemoteFavorite(favorites, endpoint, clusterID, b.Ident)
		if aFavorite != bFavorite {
			return aFavorite
		}
		aActive, bActive := a.Agents > 0, b.Agents > 0
		if aActive != bActive {
			return aActive
		}
		aLower, bLower := strings.ToLower(a.Ident), strings.ToLower(b.Ident)
		if aLower != bLower {
			return aLower < bLower
		}
		return a.Ident < b.Ident
	})
	return out
}

func projectRemoteFavorite(favorites map[string]bool, endpoint, clusterID, project string) bool {
	if clusterID != "" && favorites[clusterFavoriteKey(clusterID, project)] {
		return true
	}
	return favorites[FavoriteKey(endpoint, project)]
}

func projectMatchesSearch(ident string, aliases []string, query string, exact bool) bool {
	names := make([]string, 0, len(aliases)+1)
	names = append(names, strings.ToLower(ident))
	for _, alias := range aliases {
		if alias = strings.ToLower(strings.TrimSpace(alias)); alias != "" {
			names = append(names, alias)
		}
	}
	for _, name := range names {
		if exact && name == query || !exact && strings.Contains(name, query) {
			return true
		}
	}
	return false
}

func sortVisibleTasks(rows []storage.TaskSummary) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		aSecond := max(0, a.UpdatedAt) / 1000
		bSecond := max(0, b.UpdatedAt) / 1000
		if aSecond != bSecond {
			return aSecond > bSecond
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
}

// groupTaskRows keeps loaded children immediately after their parent without
// changing the order of unrelated tasks or the scope of the active filter.
func groupTaskRows(rows []storage.TaskSummary) []storage.TaskSummary {
	present := make(map[string]bool, len(rows))
	for _, row := range rows {
		present[row.ID] = true
	}
	children := make(map[string][]storage.TaskSummary)
	for _, row := range rows {
		if row.Parent != nil && present[row.Parent.ID] {
			children[row.Parent.ID] = append(children[row.Parent.ID], row)
		}
	}
	if len(children) == 0 {
		return rows
	}
	grouped := make([]storage.TaskSummary, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	var appendBranch func(storage.TaskSummary)
	appendBranch = func(row storage.TaskSummary) {
		if seen[row.ID] {
			return
		}
		seen[row.ID] = true
		grouped = append(grouped, row)
		for _, child := range children[row.ID] {
			appendBranch(child)
		}
	}
	for _, row := range rows {
		if row.Parent == nil || !present[row.Parent.ID] {
			appendBranch(row)
		}
	}
	for _, row := range rows {
		appendBranch(row)
	}
	return grouped
}

// FilterTasks matches every query term across task IDs, titles and labels.
// A leading # is ignored for short-ID searches; input order is preserved.
func FilterTasks(rows []storage.TaskSummary, query string) []storage.TaskSummary {
	terms := strings.Fields(normalizeTaskSearch(query))
	if len(terms) == 0 {
		return append([]storage.TaskSummary(nil), rows...)
	}
	out := make([]storage.TaskSummary, 0, len(rows))
	for _, row := range rows {
		content := strings.ToLower(row.ID + " " + row.Title + " " + strings.Join(row.Labels, " "))
		matched := true
		for _, term := range terms {
			if !strings.Contains(content, term) {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, row)
		}
	}
	return out
}

func normalizeTaskSearch(query string) string {
	return strings.TrimLeft(strings.ToLower(strings.TrimSpace(query)), "#")
}

func taskSearchIncludesAllStates(status, query string) bool {
	return status == "all" || len(strings.Fields(normalizeTaskSearch(query))) > 0
}
