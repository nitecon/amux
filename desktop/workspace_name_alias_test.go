package desktop

import (
	"context"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/storage"
)

func TestWorkspaceNamesRenameClearReloadAndSearchProjectAliases(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	b := fixture.browser
	prefsPath := filepath.Join(t.TempDir(), "navigation.json")
	b.prefsPath = prefsPath
	if err := fixture.store.InsertProject(context.Background(), storage.Project{Ident: "beta", ChannelName: "beta", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	b.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool { return projectExists(b.projectItems, "beta") })
	first := b.activeWorkspaceID
	b.workspaceName.SetText("Twin")
	if b.selection.WorkspaceName != "Twin" || b.selection.Project != "demo" {
		t.Fatalf("renaming changed project binding or failed to save name: %+v", b.selection)
	}
	b.projectSearch.SetText("Twin")
	if got := visibleProjectIDs(b.visibleProjects); !reflect.DeepEqual(got, []string{"demo"}) {
		t.Fatalf("workspace alias search = %v, want demo", got)
	}
	b.projectSearch.SetText("demo")
	if got := visibleProjectIDs(b.visibleProjects); !reflect.DeepEqual(got, []string{"demo"}) {
		t.Fatalf("gateway project search changed under alias: %v", got)
	}
	b.projectSearch.SetText("")

	fynetest.Tap(b.workspaceAdd)
	second := b.activeWorkspaceID
	if second == first {
		t.Fatal("adding a workspace reused the existing workspace ID")
	}
	for i, project := range b.visibleProjects {
		if project.Ident == "beta" {
			b.projects.Select(i)
			break
		}
	}
	if b.selectedProject != "beta" {
		t.Fatalf("second workspace did not bind beta: %q", b.selectedProject)
	}
	b.workspaceName.SetText("Twin")
	if len(b.workspaceSelect.Options) != 2 || b.workspaceSelect.Options[0] == b.workspaceSelect.Options[1] {
		t.Fatalf("duplicate names collapsed workspace options: %v", b.workspaceSelect.Options)
	}
	b.projectSearch.SetText("Twin")
	if got := visibleProjectIDs(b.visibleProjects); !reflect.DeepEqual(got, []string{"beta", "demo"}) {
		t.Fatalf("duplicate aliases did not resolve independently: %v", got)
	}
	b.workspaceName.SetText("Widget")
	b.projectSearch.SetText("Widget")
	if got := visibleProjectIDs(b.visibleProjects); !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("renaming one workspace changed alias projection: %v", got)
	}

	selectWorkspaceID(t, b, first)
	if b.workspaceName.Text != "Twin" || b.selection.Project != "demo" {
		t.Fatalf("first workspace name/binding changed with peer rename: name=%q selection=%+v", b.workspaceName.Text, b.selection)
	}
	selectWorkspaceID(t, b, second)
	if b.workspaceName.Text != "Widget" || b.selection.Project != "beta" {
		t.Fatalf("second workspace rename was not independent: name=%q selection=%+v", b.workspaceName.Text, b.selection)
	}
	b.workspaceName.SetText("")
	b.projectSearch.SetText("Widget")
	if len(b.visibleProjects) != 0 {
		t.Fatalf("cleared workspace name remained searchable: %v", visibleProjectIDs(b.visibleProjects))
	}
	if got := b.workspaceSelect.Selected; got != "Workspace 2 · beta" {
		t.Fatalf("cleared name did not restore project fallback label: %q", got)
	}
	b.persist()

	saved, err := LoadNavigationPreferences(prefsPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Selections[workspaceSelectionKey(b.prefKey, first)].WorkspaceName != "Twin" || saved.Selections[workspaceSelectionKey(b.prefKey, second)].WorkspaceName != "" {
		t.Fatalf("renamed/cleared names were not persisted independently: %+v", saved.Selections)
	}
	projectItems := append([]storage.ProjectOverview(nil), b.projectItems...)
	b.Close()
	b.Wait()

	reopened, err := NewBrowser(BrowserConfig{Client: b.client, PreferencesPath: prefsPath, Dispatch: fixture.callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close(); reopened.Wait() })
	reopened.projectItems = projectItems
	reopened.applyProjects(projectItems, nil)
	if !reflect.DeepEqual(reopened.workspaceIDs, []string{first, second}) || reopened.activeWorkspaceID != second {
		t.Fatalf("workspace identities or active selection changed after reload: ids=%v active=%q", reopened.workspaceIDs, reopened.activeWorkspaceID)
	}
	reopened.projectSearch.SetText("Twin")
	if got := visibleProjectIDs(reopened.visibleProjects); !reflect.DeepEqual(got, []string{"demo"}) {
		t.Fatalf("reloaded alias search = %v, want demo only", got)
	}
}

func visibleProjectIDs(rows []storage.ProjectOverview) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].Ident
	}
	return ids
}
