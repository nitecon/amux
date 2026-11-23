package gatewayclient

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestManifestClientReloadsPeerCredentialAfterRenewal(t *testing.T) {
	fixture := newManifestTransportFixture(t)
	credentials, err := storage.LoadPeerClientCredentials(fixture.peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewEnrolledFromManifest("", fixture.peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Projects(context.Background()); err != nil {
		t.Fatalf("initial manifest request: %v", err)
	}
	oldFingerprint := certificateFingerprint(credentials.Certificate.Leaf)
	if fixture.lastFingerprint() != oldFingerprint {
		t.Fatalf("initial request used unexpected certificate: %s", fixture.lastFingerprint())
	}

	// A damaged pending renewal is transient while the active certificate is
	// valid. The same client must continue with that current generation.
	pendingPath := filepath.Join(filepath.Dir(fixture.peer.ProfilePath), "renew-pending.json")
	if err := os.WriteFile(pendingPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Projects(context.Background()); err != nil {
		t.Fatalf("valid active identity was lost after a renewal error: %v", err)
	}
	if fixture.lastFingerprint() != oldFingerprint {
		t.Fatal("failed renewal changed the active transport")
	}
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}

	fixture.failRenewal.Store(true)
	if err := storage.RenewPeerClientNow(context.Background(), fixture.peer.ProfilePath); err == nil {
		t.Fatal("interrupted renewal reported success")
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("renewal did not persist its pending request: %v", err)
	}
	if _, err := client.Projects(context.Background()); err != nil {
		t.Fatalf("same client did not finish the pending renewal: %v", err)
	}
	updated, err := storage.LoadPeerClientCredentials(fixture.peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	newFingerprint := certificateFingerprint(updated.Certificate.Leaf)
	if newFingerprint == oldFingerprint {
		t.Fatal("automatic recovery did not install a new certificate")
	}
	if fixture.lastFingerprint() != newFingerprint {
		t.Fatalf("HTTP request used old certificate after renewal: got %s want %s", fixture.lastFingerprint(), newFingerprint)
	}

	stop := errors.New("stop after ready")
	err = client.WatchNavigation(context.Background(), func() error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("same client WebSocket after renewal: %v", err)
	}
	if fixture.lastFingerprint() != newFingerprint {
		t.Fatalf("WebSocket used old certificate after renewal: got %s want %s", fixture.lastFingerprint(), newFingerprint)
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	fixture.mu.Lock()
	fixture.redirectURL = target.URL
	fixture.mu.Unlock()
	fixture.redirect.Store(true)
	_, err = client.Projects(context.Background())
	var httpError *HTTPError
	if !errors.As(err, &httpError) || httpError.Status != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("manifest client followed credential redirect: err=%v hits=%d", err, redirected.Load())
	}
}

func TestManifestClientExposesPinnedClusterIdentity(t *testing.T) {
	fixture := newManifestTransportFixture(t)
	credentials, err := storage.LoadPeerClientCredentials(fixture.peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewEnrolledFromManifest("", fixture.peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.ClusterID(); got == "" || got != credentials.Profile.ClusterID {
		t.Fatalf("trusted cluster identity = %q, want pinned profile identity %q", got, credentials.Profile.ClusterID)
	}
	ordinary, err := New("https://127.0.0.1", "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.ClusterID() != "" {
		t.Fatalf("ordinary client unexpectedly has authenticated cluster identity %q", ordinary.ClusterID())
	}
}

func TestManifestClientFailsClosedOnIdentityRebindAndBlocksRedirects(t *testing.T) {
	fixture := newManifestTransportFixture(t)
	client, err := NewEnrolledFromManifest("", fixture.peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := storage.LoadPeerClientCredentials(fixture.peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Projects(context.Background()); err != nil {
		t.Fatal(err)
	}
	requestsBeforeRebind := fixture.requests.Load()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.RenewPeerClientNow(canceled, fixture.peer.ProfilePath); err == nil {
		t.Fatal("canceled renewal unexpectedly completed")
	}
	renewalCalls := atomic.Int32{}
	wrongOrigin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		renewalCalls.Add(1)
		http.NotFound(w, nil)
	}))
	wrongOrigin.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{fixture.serverCertificate}}
	wrongOrigin.StartTLS()
	defer wrongOrigin.Close()
	changed := credentials.Profile
	changed.URL = wrongOrigin.URL
	changed.InstanceID = uuid.NewString()
	data, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.peer.ProfilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Projects(context.Background()); err == nil {
		t.Fatal("manifest identity rebind was accepted")
	}
	if got := fixture.requests.Load(); got != requestsBeforeRebind {
		t.Fatalf("rebound manifest reached gateway: requests=%d want %d", got, requestsBeforeRebind)
	}
	if got := renewalCalls.Load(); got != 0 {
		t.Fatalf("changed manifest caused renewal traffic to its unpinned origin: %d requests", got)
	}
}

type manifestTransportFixture struct {
	peer              storage.PeerProfile
	serverCertificate tls.Certificate
	mu                sync.Mutex
	fingerprint       string
	redirect          atomic.Bool
	failRenewal       atomic.Bool
	requests          atomic.Int32
	redirectURL       string
}

func (f *manifestTransportFixture) lastFingerprint() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fingerprint
}

func newManifestTransportFixture(t *testing.T) *manifestTransportFixture {
	t.Helper()
	ctx := context.Background()
	profileDir := t.TempDir()
	enrollment, err := storage.OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	discoveryListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		enrollment.Close()
		t.Fatal(err)
	}
	secureListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		discoveryListener.Close()
		enrollment.Close()
		t.Fatal(err)
	}
	secureAddress := secureListener.Addr().String()
	secureURL := "https://" + secureAddress
	discoveryPort := uint16(discoveryListener.Addr().(*net.TCPAddr).Port)
	admin, err := enrollment.ProvisionLocal(ctx, secureURL, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	clusterID := enrollment.Discovery(uint16(secureListener.Addr().(*net.TCPAddr).Port)).ClusterID
	serverCertificate, err := storage.EnsureProfileServerTLS(profileDir, clusterID, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, err := os.ReadFile(filepath.Join(profileDir, "root.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatal("invalid fixture root")
	}
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &manifestTransportFixture{serverCertificate: serverCertificate}
	config := server.HandlerConfig{
		SecurityEnrollment: enrollment, LocalNodeID: admin.NodeID, DiscoveryPort: discoveryPort,
		ProcessNonce: uuid.NewString(), SecureBind: secureAddress, DiscoveryBind: discoveryListener.Addr().String(),
		Shutdown: func() {},
	}
	handler, err := server.NewHandlerWithConfig(store, "manifest-test-key", config)
	if err != nil {
		t.Fatal(err)
	}
	recordingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/enrollment/renew" && fixture.failRenewal.Swap(false) {
			http.Error(w, "synthetic interruption", http.StatusServiceUnavailable)
			return
		}
		fixture.mu.Lock()
		redirectURL := fixture.redirectURL
		fixture.mu.Unlock()
		if fixture.redirect.Load() && r.URL.Path == "/v1/projects" {
			http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
			return
		}
		fixture.requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "bearer credential unexpectedly present", http.StatusUnauthorized)
			return
		}
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			fixture.mu.Lock()
			fixture.fingerprint = certificateFingerprint(r.TLS.PeerCertificates[0])
			fixture.mu.Unlock()
		}
		handler.ServeHTTP(w, r)
	})
	secureServer := &http.Server{Handler: recordingHandler}
	discoveryMux := http.NewServeMux()
	discoveryMux.HandleFunc("GET /v1/discovery", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(enrollment.Discovery(uint16(secureListener.Addr().(*net.TCPAddr).Port)))
	})
	discoveryServer := &http.Server{Handler: discoveryMux}
	secureTLS := tls.NewListener(secureListener, &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverCertificate},
		ClientCAs: roots, ClientAuth: tls.VerifyClientCertIfGiven,
	})
	secureDone := make(chan error, 1)
	discoveryDone := make(chan error, 1)
	go func() { secureDone <- secureServer.Serve(secureTLS) }()
	go func() { discoveryDone <- discoveryServer.Serve(discoveryListener) }()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = secureServer.Shutdown(shutdown)
		_ = discoveryServer.Shutdown(shutdown)
		<-secureDone
		<-discoveryDone
		_ = store.Close()
		_ = enrollment.Close()
	})
	invitation, err := enrollment.IssueInvitation(discoveryPort)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := storage.PairRemote(ctx, t.TempDir(), secureURL, invitation.Invitation)
	if err != nil {
		t.Fatal(err)
	}
	fixture.peer = peer
	return fixture
}

func certificateFingerprint(certificate *x509.Certificate) string {
	digest := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(digest[:])
}
