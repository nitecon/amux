package desktop

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

func TestProjectFavoriteReconcilesLocalCheckoutAfterProjectResolution(t *testing.T) {
	endpoint := "https://gateway.example"
	checkout := filepath.Join(t.TempDir(), "checkout")
	local := projectFavoriteKeys(endpoint, checkout, "")
	favorites := map[string]bool{local[0]: true}
	resolved := projectFavoriteKeys(endpoint, checkout, "Project-X")
	if !reconcileProjectFavorite(favorites, resolved) {
		t.Fatal("local checkout favorite was not reconciled after project resolution")
	}
	if len(favorites) != 1 || !favorites[FavoriteKey(endpoint, "project-x")] || favorites[local[0]] {
		t.Fatalf("reconciled favorites = %v, want one endpoint-scoped project key", favorites)
	}
	toggleProjectFavorite(favorites, resolved)
	if len(favorites) != 0 {
		t.Fatalf("toggle left favorite aliases behind: %v", favorites)
	}
}

func TestProjectFavoriteKeysKeepEndpointScopeAndCanonicalCheckoutPath(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "work", "repo")
	keys := projectFavoriteKeys("https://one.example", checkout, "Demo")
	if len(keys) != 2 || keys[0] != "local:"+checkout {
		t.Fatalf("favorite keys = %v", keys)
	}
	favorites := map[string]bool{FavoriteKey("https://one.example", "demo"): true}
	if len(projectFavoriteKeys("https://two.example", checkout, "demo")) != 2 || favorites[FavoriteKey("https://two.example", "demo")] {
		t.Fatal("remote favorite crossed gateway endpoint scope")
	}
}

func TestClusterFavoriteMigrationAndEndpointFallback(t *testing.T) {
	endpoint := "https://gateway-one.example"
	legacy := FavoriteKey(endpoint, "Project-X")
	cluster := clusterFavoriteKey("trusted-cluster", "project-x")
	favorites := map[string]bool{legacy: true, FavoriteKey("https://gateway-two.example", "project-x"): true}
	if migrateEndpointFavorites(favorites, endpoint, "") || migrateEndpointFavorites(favorites, endpoint, "") {
		t.Fatal("endpoint favorites migrated without a trusted cluster identity")
	}
	if !migrateEndpointFavorites(favorites, endpoint, "trusted-cluster") {
		t.Fatal("trusted cluster did not migrate the endpoint favorite")
	}
	if !favorites[cluster] || favorites[legacy] || !favorites[FavoriteKey("https://gateway-two.example", "project-x")] {
		t.Fatalf("cluster migration lost fallback or crossed endpoint scope: %v", favorites)
	}
	if clusterFavoriteKey("other-cluster", "project-x") == cluster {
		t.Fatal("cluster favorite identity crossed cluster boundaries")
	}
}

func TestBrowserUsesTrustedEnrolledClusterAndMigratesOldFavorite(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	ctx := context.Background()
	profileDir := t.TempDir()
	enrollment, err := storage.OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enrollment.Close() })
	profile, err := enrollment.ProvisionLocal(ctx, "https://127.0.0.1:1", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	client, err := gatewayclient.NewEnrolledFromManifest("", filepath.Join(profileDir, "local-client.json"))
	if err != nil {
		t.Fatal(err)
	}
	if client.ClusterID() != profile.ClusterID {
		t.Fatalf("client cluster identity = %q, want authenticated profile cluster %q", client.ClusterID(), profile.ClusterID)
	}
	prefsPath := filepath.Join(t.TempDir(), "navigation.json")
	oldEndpointFavorite := FavoriteKey(client.Endpoint(), "demo")
	if err := SaveNavigationPreferences(prefsPath, NavigationPreferences{Favorites: map[string]bool{oldEndpointFavorite: true}}); err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserConfig{Client: client, PreferencesPath: prefsPath, Dispatch: func(fn func()) { fn() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait() })
	clusterKey := clusterFavoriteKey(profile.ClusterID, "demo")
	if browser.clusterID != profile.ClusterID || !browser.prefs.Favorites[clusterKey] || browser.prefs.Favorites[oldEndpointFavorite] {
		t.Fatalf("browser did not migrate under trusted cluster identity: cluster=%q favorites=%v", browser.clusterID, browser.prefs.Favorites)
	}
	saved, err := LoadNavigationPreferences(prefsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Favorites[clusterKey] || saved.Favorites[oldEndpointFavorite] {
		t.Fatalf("cluster-scoped migration was not persisted: %v", saved.Favorites)
	}
	if !projectRemoteFavorite(saved.Favorites, "https://replacement.example", profile.ClusterID, "demo") ||
		projectRemoteFavorite(saved.Favorites, client.Endpoint(), "other-cluster", "demo") {
		t.Fatalf("saved favorite did not follow only its enrolled cluster: %v", saved.Favorites)
	}
	browser.projectItems = []storage.ProjectOverview{
		{Project: storage.Project{Ident: "alpha"}},
		{Project: storage.Project{Ident: "demo"}},
	}
	browser.applyProjectFilter()
	if len(browser.visibleProjects) != 2 || browser.visibleProjects[0].Ident != "demo" || browser.favoriteText("demo") != "Unfavorite" {
		t.Fatalf("cluster-scoped favorite was not reflected in project navigation: %+v", browser.visibleProjects)
	}
}

func TestToggleLocalFavoriteWithoutResolvedProject(t *testing.T) {
	keys := projectFavoriteKeys("https://gateway.example", filepath.Join(t.TempDir(), "checkout"), "")
	favorites := map[string]bool{}
	toggleProjectFavorite(favorites, keys)
	if len(favorites) != 1 || !favorites[keys[0]] {
		t.Fatalf("local favorite = %v, want checkout identity only", favorites)
	}
}

func TestBrowserProjectResolutionReconcilesSavedCheckoutFavorite(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	b := fixture.browser
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", checkout, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("initialize checkout: %s: %v", output, err)
	}
	if output, err := exec.Command("git", "-C", checkout, "remote", "add", "origin", "git@github.com:example/widget.git").CombinedOutput(); err != nil {
		t.Fatalf("set checkout origin: %s: %v", output, err)
	}
	identity, err := projectid.ResolveCheckout(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := projectid.CloneOrigin(context.Background(), identity.Directory)
	if err != nil || origin == nil {
		t.Fatalf("resolve checkout origin: %v %v", origin, err)
	}
	matchedRemote := identity.Upstream
	unmatchedRemote, err := projectid.CanonicalRemote("github.com/example/other")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.InsertProject(context.Background(), storage.Project{Ident: "000-unrelated", ChannelName: "unrelated", Kind: "repo", CanonicalRemote: &unmatchedRemote}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.InsertProject(context.Background(), storage.Project{Ident: "zzz-verified", ChannelName: "widget", Kind: "repo", CanonicalRemote: &matchedRemote}); err != nil {
		t.Fatal(err)
	}
	prefsPath := filepath.Join(t.TempDir(), "navigation.json")
	b.prefsPath = prefsPath
	b.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool {
		return projectExists(b.projectItems, "000-unrelated") && projectExists(b.projectItems, "zzz-verified")
	})
	b.selection.Project = ""
	b.selection.CheckoutPath = identity.Directory
	b.selection.CheckoutOrigin = origin
	b.selectedProject = ""
	b.prefs.Selections[workspaceSelectionKey(b.prefKey, b.activeWorkspaceID)] = b.selection
	localKeys := projectFavoriteKeys(b.endpoint, identity.Directory, "")
	fynetest.Tap(b.selectedProjectFavorite)
	if !b.prefs.Favorites[localKeys[0]] {
		t.Fatal("favorite button did not pin the unresolved local checkout")
	}
	b.persist()

	b.applyProjects(b.projectItems, nil)
	if b.selectedProject != "000-unrelated" || !b.prefs.Favorites[localKeys[0]] || len(b.prefs.Favorites) != 1 {
		t.Fatalf("unmatched first project consumed the local favorite: selected=%q favorites=%v", b.selectedProject, b.prefs.Favorites)
	}
	workspaceID := b.activeWorkspaceID
	for i, item := range b.visibleProjects {
		if item.Ident == "zzz-verified" {
			b.projects.Select(i)
			break
		}
	}
	if b.selectedProject != "zzz-verified" {
		t.Fatalf("could not select matching gateway project: %q", b.selectedProject)
	}
	binding := b.workspaceBindingGeneration[workspaceID]
	resolved, err := projectid.ResolveCheckout(context.Background(), identity.Directory)
	if err != nil {
		t.Fatal(err)
	}
	var matchingProject *storage.ProjectOverview
	for i := range b.projectItems {
		if b.projectItems[i].Ident == "zzz-verified" {
			matchingProject = &b.projectItems[i]
			break
		}
	}
	if matchingProject == nil {
		t.Fatal("matching gateway project disappeared from inventory")
	}
	if err := validateWorkspaceCheckoutProject(*matchingProject, resolved); err != nil {
		t.Fatalf("fixture checkout is not an exact project match: %v", err)
	}
	if !b.bindWorkspaceCheckoutWithOrigin(workspaceID, binding, "zzz-verified", resolved.Directory, origin) {
		t.Fatalf("verified checkout attach rejected (captured binding generation %d)", binding)
	}
	remoteKey := FavoriteKey(b.endpoint, "zzz-verified")
	saved, err := LoadNavigationPreferences(prefsPath)
	if err != nil {
		t.Fatal(err)
	}
	if b.prefs.Favorites[localKeys[0]] || !b.prefs.Favorites[remoteKey] || len(b.prefs.Favorites) != 1 || !saved.Favorites[remoteKey] {
		t.Fatalf("verified project did not acquire and persist the checkout favorite: favorites=%v saved=%v", b.prefs.Favorites, saved.Favorites)
	}
	if b.favoriteText("zzz-verified") != "Unfavorite" {
		t.Fatal("active project favorite button did not reflect reconciled identity")
	}
	fynetest.Tap(b.selectedProjectFavorite)
	if len(b.prefs.Favorites) != 0 {
		t.Fatalf("favorite toggle left a local or remote alias behind: %v", b.prefs.Favorites)
	}
	if b.favoriteText("zzz-verified") != "Favorite" {
		t.Fatal("favorite button did not reflect the unpinned project")
	}
}
