package desktop

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

func TestProjectCheckoutUsesExactLocationAndReusesExistingFolder(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.git")
	if output, err := exec.Command("git", "init", "--bare", source).CombinedOutput(); err != nil {
		t.Fatalf("prepare Git source: %v: %s", err, output)
	}
	project := storage.ProjectOverview{Project: storage.Project{Ident: "demo", Kind: "adhoc"}}
	destination := filepath.Join(root, "checkout with spaces")
	for _, test := range []struct {
		action, folder, want string
	}{
		{cloneAtLocation, destination, destination},
		{openInFolder, destination, destination},
		{cloneInProjects, "", filepath.Join(root, "source")},
	} {
		result, _, err := prepareProjectCheckout(context.Background(), project, test.action, root, test.folder, source)
		if err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(test.want)
		if err != nil || result.Directory != want {
			t.Fatalf("%s resolved to %q, want %q: %v", test.action, result.Directory, want, err)
		}
	}
}

func TestProjectsDirectoryDiscoveryMatchesGatewayByOrigin(t *testing.T) {
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler { return handler })
	b := fixture.browser
	parent := t.TempDir()
	checkout := filepath.Join(parent, "custom folder")
	if err := os.Mkdir(checkout, 0755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"remote", "add", "origin", "git@github.com:nitecon/amux.git"}} {
		if output, err := exec.Command("git", append([]string{"-C", checkout}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("prepare local checkout: %v: %s", err, output)
		}
	}
	if err := b.saveProjectsDirectory(parent); err != nil {
		t.Fatal(err)
	}
	project := storage.ProjectOverview{Project: storage.Project{Ident: "amux", CanonicalRemote: stringPtrDesktop("github.com/nitecon/amux")}}
	want, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.projectCheckout(project).Directory == want
	})
	b.clientConfigPath = filepath.Join(t.TempDir(), "client.yml")
	b.projectItems = []storage.ProjectOverview{project}
	b.selectedProject = project.Ident
	b.selection.CheckoutPath = ""
	b.applyProjectFilter()
	b.updateSelectedProjectStatus()
	if binding := b.projectCheckout(project); binding.Directory != "" {
		t.Fatalf("discovery created a managed binding: %+v", binding)
	}
	if status := b.selectedProjectStatus.Text; !strings.Contains(status, want+" (found locally, awaiting attachment)") {
		t.Fatalf("managed checkout discovery was hidden: %q", status)
	}
	b.prefKey = "https://another.example"
	if b.projectsDirectory() != parent {
		t.Fatalf("projects directory changed with gateway: %q", b.projectsDirectory())
	}
}

func TestProjectContextMenuOpensCheckoutPanel(t *testing.T) {
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler { return handler })
	b := fixture.browser
	row := b.projects.CreateItem().(*projectMenuRow)
	b.projects.UpdateItem(0, row)
	window := fynetest.NewWindow(row)
	t.Cleanup(window.Close)
	fynetest.TapSecondary(row)
	overlay, ok := window.Canvas().Overlays().Top().(fyne.Widget)
	if !ok {
		t.Fatal("right-click did not open the project menu")
	}
	var checkout *widget.Button
	for _, object := range fynetest.WidgetRenderer(overlay).Objects() {
		if menu, ok := object.(*widget.PopUp); ok {
			checkout = findSettingsButton(menu.Content, "Check out…")
		}
	}
	if checkout == nil {
		t.Fatal("project context menu is missing checkout")
	}
	fynetest.Tap(checkout)
	if window.Canvas().Overlays().Top() != nil || b.mainView != "Checkout" {
		t.Fatal("checkout should dismiss the menu and open the panel")
	}
}

func TestProjectRailGroupsAndFiltersLocalAndGatewayProjects(t *testing.T) {
	fixture := startEditorBrowser(t, func(handler http.Handler) http.Handler { return handler })
	b := fixture.browser
	local := filepath.Join(t.TempDir(), "amux-local")
	b.localCheckouts = []projectid.LocalCheckout{{Directory: local}}
	b.projectItems = []storage.ProjectOverview{
		{Project: storage.Project{Ident: "amux-favorite"}},
		{Project: storage.Project{Ident: "amux-remote"}},
	}
	b.prefs.Favorites[FavoriteKey(b.endpoint, "amux-favorite")] = true
	b.selection.ProjectFilter = "amux"
	b.applyProjectFilter()
	want := []string{"FAVORITES", "LOCAL PROJECTS", "GATEWAY PROJECTS"}
	if len(b.visibleProjects) != len(want) {
		t.Fatalf("filtered project count: got %d, want %d", len(b.visibleProjects), len(want))
	}
	for i, group := range want {
		if got := b.projectGroupAt(i); got != group {
			t.Fatalf("project group %d: got %q, want %q", i, got, group)
		}
	}
	b.selectRailProject(b.visibleProjects[1].Ident)
	if b.mainView != "Files" || b.selectedProjectStatus.Text != "Selected Project: amux-local  |  Path: "+local {
		t.Fatalf("local project selection: view=%s status=%q", b.mainView, b.selectedProjectStatus.Text)
	}
	b.selectRailProject("amux-remote")
	if b.selectedRailProject() != "amux-remote" || !strings.Contains(b.selectedProjectStatus.Text, "No local checkout") {
		t.Fatalf("gateway project selection: id=%q status=%q", b.selectedRailProject(), b.selectedProjectStatus.Text)
	}
}
