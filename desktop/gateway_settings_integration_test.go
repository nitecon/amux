package desktop

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	_ "modernc.org/sqlite"
)

type desktopGatewayFixture struct {
	profile           string
	endpoint          string
	port              int
	instanceID        string
	enroll            *storage.SecurityEnrollment
	store             *storage.Store
	locks             []*os.File
	cancel            context.CancelFunc
	secure            chan error
	discovery         *http.Server
	discoveryListener net.Listener
	discoveryDone     chan error
	stopOnce          sync.Once
}

func startDesktopGatewayFixture(t *testing.T, profile string, port int, seedProject bool, instanceID string) *desktopGatewayFixture {
	t.Helper()
	ctx := context.Background()
	securityLock, err := server.LockSecurityProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := storage.EnsureSecurityInstallation(ctx, profile)
	if err != nil {
		securityLock.Close()
		t.Fatal(err)
	}
	if err := storage.EnsureSecurityRoot(profile, cluster); err != nil {
		securityLock.Close()
		t.Fatal(err)
	}
	enrollment, err := storage.OpenSecurityEnrollment(ctx, profile)
	if err != nil {
		securityLock.Close()
		t.Fatal(err)
	}
	listen := "127.0.0.1:0"
	if port != 0 {
		listen = fmt.Sprintf("127.0.0.1:%d", port)
	}
	rawListener, err := net.Listen("tcp", listen)
	if err != nil {
		enrollment.Close()
		securityLock.Close()
		t.Fatal(err)
	}
	securePort := rawListener.Addr().(*net.TCPAddr).Port
	discoveryListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		rawListener.Close()
		enrollment.Close()
		securityLock.Close()
		t.Fatal(err)
	}
	discoveryPort := uint16(discoveryListener.Addr().(*net.TCPAddr).Port)
	endpoint := fmt.Sprintf("https://127.0.0.1:%d", securePort)
	if instanceID == "" {
		instanceID = uuid.NewString()
	}
	admin, err := enrollment.ProvisionLocal(ctx, endpoint, instanceID)
	if err != nil {
		rawListener.Close()
		discoveryListener.Close()
		enrollment.Close()
		securityLock.Close()
		t.Fatal(err)
	}
	certificate, err := storage.EnsureProfileServerTLS(profile, cluster, []string{"127.0.0.1"})
	if err != nil {
		rawListener.Close()
		discoveryListener.Close()
		enrollment.Close()
		securityLock.Close()
		t.Fatal(err)
	}
	rootPEM, err := os.ReadFile(filepath.Join(profile, "root.pem"))
	if err != nil {
		t.Fatal(err)
	}
	clientRoots := x509.NewCertPool()
	if !clientRoots.AppendCertsFromPEM(rootPEM) {
		t.Fatal("profile root PEM could not be loaded")
	}
	appLock, err := server.LockProfile(filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, filepath.Join(profile, "application.sqlite"))
	if err != nil {
		appLock.Close()
		t.Fatal(err)
	}
	if seedProject {
		if err := store.InsertProject(ctx, storage.Project{Ident: "gateway-ui", ChannelName: "gateway-ui", Kind: "adhoc"}); err != nil {
			store.Close()
			appLock.Close()
			t.Fatal(err)
		}
	}
	config := server.HandlerConfig{
		SecurityEnrollment: enrollment, LocalNodeID: admin.NodeID, DiscoveryPort: discoveryPort,
		ProcessNonce: uuid.NewString(), SecureBind: rawListener.Addr().String(), DiscoveryBind: discoveryListener.Addr().String(), Shutdown: func() {},
	}
	handler, err := server.NewHandlerWithConfig(store, "gateway-fixture-only", config)
	if err != nil {
		store.Close()
		appLock.Close()
		t.Fatal(err)
	}
	secureListener, _, err := server.TrackSecureConnections(rawListener, &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate},
		ClientCAs: clientRoots, ClientAuth: tls.VerifyClientCertIfGiven,
	})
	if err != nil {
		store.Close()
		appLock.Close()
		t.Fatal(err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	secureDone := make(chan error, 1)
	go func() { secureDone <- server.Serve(serverCtx, secureListener, handler, 2*time.Second) }()
	discoveryInfo := enrollment.Discovery(uint16(securePort))
	discovery := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/discovery" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(discoveryInfo)
	})}
	discoveryDone := make(chan error, 1)
	go func() { discoveryDone <- discovery.Serve(discoveryListener) }()
	fixture := &desktopGatewayFixture{
		profile: profile, endpoint: endpoint, port: securePort, instanceID: instanceID, enroll: enrollment, store: store,
		locks: []*os.File{securityLock, appLock}, cancel: cancel, secure: secureDone,
		discovery: discovery, discoveryListener: discoveryListener, discoveryDone: discoveryDone,
	}
	t.Cleanup(func() { fixture.stop(t) })
	return fixture
}

func (f *desktopGatewayFixture) stop(t *testing.T) {
	t.Helper()
	f.stopOnce.Do(func() {
		f.cancel()
		_ = f.discovery.Shutdown(context.Background())
		if err := <-f.secure; err != nil {
			t.Errorf("secure gateway shutdown: %v", err)
		}
		if err := <-f.discoveryDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("discovery shutdown: %v", err)
		}
		if err := f.store.Close(); err != nil {
			t.Errorf("close gateway storage: %v", err)
		}
		if err := f.enroll.Close(); err != nil {
			t.Errorf("close gateway enrollment: %v", err)
		}
		for _, lock := range f.locks {
			if err := lock.Close(); err != nil {
				t.Errorf("release gateway profile lock: %v", err)
			}
		}
	})
}

func gatewayAdminClient(t *testing.T, profile string) *http.Client {
	t.Helper()
	credentials, err := storage.LoadLocalClientCredentials(profile)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: credentials.RootCAs, Certificates: []tls.Certificate{credentials.Certificate},
	}}}
}

func requestGatewayInvitation(t *testing.T, client *http.Client, endpoint, method string) (storage.Invitation, int) {
	t.Helper()
	request, err := http.NewRequest(method, endpoint+"/v1/server/invitation", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var invitation storage.Invitation
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&invitation); err != nil {
			t.Fatal(err)
		}
	}
	return invitation, response.StatusCode
}

func TestGatewaySettingsEnrollmentPersistsAuthenticatingPeerAcrossRestart(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "server-profile")
	stateDir := filepath.Join(t.TempDir(), "client-state")
	prefsPath := filepath.Join(t.TempDir(), "gateway.json")
	host := startDesktopGatewayFixture(t, profile, 0, true, "")
	admin := gatewayAdminClient(t, profile)
	defer admin.CloseIdleConnections()
	first, status := requestGatewayInvitation(t, admin, host.endpoint, http.MethodPost)
	if status != http.StatusOK || first.Invitation == "" {
		t.Fatalf("generate invitation status=%d invitation=%v", status, first)
	}
	copied, status := requestGatewayInvitation(t, admin, host.endpoint, http.MethodGet)
	if status != http.StatusOK || copied.Invitation != first.Invitation {
		t.Fatalf("copy invitation status=%d got=%v want same active key", status, copied)
	}
	prefs, err := completeGatewayEnrollment(context.Background(), stateDir, prefsPath, DefaultGatewayPreferences(), host.endpoint, copied.Invitation)
	if err != nil {
		t.Fatal(err)
	}
	if prefs.Mode != GatewayModeClient || prefs.EnrolledProfile == "" || prefs.RemoteHosting {
		t.Fatalf("enrollment preferences: %+v", prefs)
	}
	saved, err := os.ReadFile(prefsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved, []byte(first.Invitation)) {
		t.Fatal("raw peering key was persisted with gateway preferences")
	}
	peerClient, err := gatewayclient.NewEnrolledFromManifest(prefs.Endpoint, prefs.EnrolledProfile)
	if err != nil {
		t.Fatal(err)
	}
	projectsBefore, err := peerClient.Projects(context.Background())
	if err != nil || len(projectsBefore) != 1 || projectsBefore[0].Ident != "gateway-ui" {
		t.Fatalf("saved peer manifest could not authenticate: projects=%+v err=%v", projectsBefore, err)
	}
	second, status := requestGatewayInvitation(t, admin, host.endpoint, http.MethodPost)
	if status != http.StatusOK || second.Invitation == "" || second.Invitation == first.Invitation {
		t.Fatalf("new invitation was not generated: status=%d got=%v", status, second)
	}
	secondCopy, status := requestGatewayInvitation(t, admin, host.endpoint, http.MethodGet)
	if status != http.StatusOK || secondCopy.Invitation != second.Invitation {
		t.Fatalf("current invitation was not reusable: status=%d got=%v", status, secondCopy)
	}
	securityDB, err := sql.Open("sqlite", filepath.Join(profile, "security.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := securityDB.Exec("UPDATE invitations SET expires_at=?", time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		securityDB.Close()
		t.Fatal(err)
	}
	if err := securityDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, status := requestGatewayInvitation(t, admin, host.endpoint, http.MethodGet); status != http.StatusNotFound {
		t.Fatalf("expired invitation copy status=%d, want 404", status)
	}
	port := host.port
	host.stop(t)
	restarted := startDesktopGatewayFixture(t, profile, port, false, host.instanceID)
	peerAfterRestart, err := gatewayclient.NewEnrolledFromManifest(prefs.Endpoint, prefs.EnrolledProfile)
	if err != nil {
		t.Fatal(err)
	}
	projectsAfter, err := peerAfterRestart.Projects(context.Background())
	if err != nil || len(projectsAfter) != 1 || projectsAfter[0].Ident != "gateway-ui" {
		t.Fatalf("saved peer manifest failed after gateway restart: projects=%+v err=%v", projectsAfter, err)
	}
	adminAfter := gatewayAdminClient(t, profile)
	defer adminAfter.CloseIdleConnections()
	if _, status := requestGatewayInvitation(t, adminAfter, restarted.endpoint, http.MethodGet); status != http.StatusNotFound {
		t.Fatalf("invitation survived a secure server restart: status=%d", status)
	}
}
