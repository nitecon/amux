package desktop

import (
	"reflect"
	"testing"

	"github.com/nitecon/amux/storage"
)

func projectRow(ident string, agents int64) storage.ProjectOverview {
	return storage.ProjectOverview{Project: storage.Project{Ident: ident}, Agents: agents}
}

func TestFilterProjectsExactFirstFavoriteAgentAndStableIdentityOrder(t *testing.T) {
	rows := []storage.ProjectOverview{
		projectRow("zeta", 0),
		projectRow("starx-api", 3),
		projectRow("StarX", 0),
		projectRow("alpha", 1),
	}
	before := append([]storage.ProjectOverview(nil), rows...)
	favorites := map[string]bool{FavoriteKey("https://gateway.test/root", "zeta"): true}
	got := FilterProjects(rows, favorites, "https://gateway.test/root", " STARX ")
	if want := []string{"StarX"}; !reflect.DeepEqual(projectIdents(got), want) {
		t.Fatalf("exact-first filter = %v, want %v", projectIdents(got), want)
	}
	got = FilterProjects(rows, favorites, "https://gateway.test/root", "")
	if want := []string{"zeta", "alpha", "starx-api", "StarX"}; !reflect.DeepEqual(projectIdents(got), want) {
		t.Fatalf("favorite/agent/alphabetical order = %v, want %v", projectIdents(got), want)
	}
	if !reflect.DeepEqual(rows, before) {
		t.Fatalf("FilterProjects mutated input: got %#v, want %#v", rows, before)
	}
}

func TestFavoriteKeyIsStableAndEndpointScoped(t *testing.T) {
	key := FavoriteKey("https://one.test/api", " Project-X ")
	if key != FavoriteKey("https://one.test/api", "project-x") {
		t.Fatalf("canonical project key changed: %q", key)
	}
	if key == FavoriteKey("https://two.test/api", "project-x") {
		t.Fatal("favorite key crossed endpoint boundary")
	}
	rows := []storage.ProjectOverview{projectRow("project-x", 0)}
	favorites := map[string]bool{key: true}
	if got := FilterProjects(rows, favorites, "https://two.test/api", ""); !reflect.DeepEqual(got, rows) {
		t.Fatal("favorite unexpectedly applied to a different endpoint")
	}
}

func projectIdents(rows []storage.ProjectOverview) []string {
	idents := make([]string, len(rows))
	for i := range rows {
		idents[i] = rows[i].Ident
	}
	return idents
}

func TestFilterTasksIncludesExactAndPartialMatchesInInputOrder(t *testing.T) {
	rows := []storage.TaskSummary{
		{ID: "id-1", Title: "Prepare release"},
		{ID: "prepare", Title: "Different title"},
		{ID: "id-3", Title: "prepare the notes"},
	}
	before := append([]storage.TaskSummary(nil), rows...)
	got := FilterTasks(rows, " PREPARE ")
	if want := []string{"id-1", "prepare", "id-3"}; !reflect.DeepEqual(taskIDs(got), want) {
		t.Fatalf("exact and partial matches = %v, want %v", taskIDs(got), want)
	}
	got = FilterTasks(rows, "release")
	if want := []string{"id-1"}; !reflect.DeepEqual(taskIDs(got), want) {
		t.Fatalf("substring filter = %v, want %v", taskIDs(got), want)
	}
	got = FilterTasks(rows, "  ")
	if !reflect.DeepEqual(got, rows) || !reflect.DeepEqual(rows, before) {
		t.Fatal("empty filtering changed task order or input rows")
	}
	if taskSearchIncludesAllStates("todo", " ##  ") || !taskSearchIncludesAllStates("todo", " #id-1 ") {
		t.Fatal("task search scope disagrees with short-ID query normalization")
	}
}

func TestFilterTasksMatchesShortIDsTitleTermsAndLabels(t *testing.T) {
	rows := []storage.TaskSummary{
		{ID: "01a16c87-1234-4567-890a-123456789abc", Title: "Windows integration check", Labels: []string{"os:windows"}},
		{ID: "task-2", Title: "Linux terminal", Status: "todo"},
		{ID: "task-3", Title: "Windows check complete", Status: "done", Labels: []string{"bug"}},
	}
	for query, want := range map[string][]string{
		" #01A16C87 ":                          {"01a16c87-1234-4567-890a-123456789abc"},
		"01a16c87-1234-4567-890a-123456789abc": {"01a16c87-1234-4567-890a-123456789abc"},
		"check WINDOWS":                        {"01a16c87-1234-4567-890a-123456789abc", "task-3"},
		"os:WINDOWS":                           {"01a16c87-1234-4567-890a-123456789abc"},
		"bug":                                  {"task-3"},
		"missing":                              {},
	} {
		if got := taskIDs(FilterTasks(rows, query)); !reflect.DeepEqual(got, want) {
			t.Errorf("FilterTasks(%q) = %v, want %v", query, got, want)
		}
	}
}

func taskIDs(rows []storage.TaskSummary) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}
