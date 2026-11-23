package storage_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type pairingFixture struct {
	profileDir       string
	stateDir         string
	storePath        string
	admin            storage.LocalSecurityProfile
	enrollment       *storage.SecurityEnrollment
	store            *storage.Store
	handler          http.Handler
	secure           *http.Server
	discovery        *http.Server
	securePort       uint16
	discoveryPort    uint16
	invite           storage.Invitation
	losePost         atomic.Bool
	enrollmentPosts  atomic.Int32
	receiptSuccesses atomic.Int32
}

func newPairingFixture(t *testing.T) *pairingFixture {
	t.Helper()
	ctx := context.Background()
	profileDir := t.TempDir()
	enrollment, err := storage.OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	discoveryListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secureListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		discoveryListener.Close()
		enrollment.Close()
		t.Fatal(err)
	}
	discoveryPort := uint16(discoveryListener.Addr().(*net.TCPAddr).Port)
	securePort := uint16(secureListener.Addr().(*net.TCPAddr).Port)
	discoveryInfo := enrollment.Discovery(securePort)
	localURL := fmt.Sprintf("https://127.0.0.1:%d", securePort)
	admin, err := enrollment.ProvisionLocal(ctx, localURL, uuid.NewString())
	if err != nil {
		discoveryListener.Close()
		secureListener.Close()
		enrollment.Close()
		t.Fatal(err)
	}
	serverCertificate, err := storage.EnsureProfileServerTLS(profileDir, discoveryInfo.ClusterID, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "application.sqlite")
	store, err := storage.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	config := server.HandlerConfig{
		SecurityEnrollment: enrollment, LocalNodeID: admin.NodeID, DiscoveryPort: discoveryPort,
		ProcessNonce: uuid.NewString(), SecureBind: secureListener.Addr().String(), Shutdown: func() {},
	}
	handler, err := server.NewHandlerWithConfig(store, "fixture-only-key", config)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &pairingFixture{
		profileDir: profileDir, stateDir: t.TempDir(), storePath: storePath, admin: admin,
		enrollment: enrollment, store: store, handler: handler,
		securePort: securePort, discoveryPort: discoveryPort,
	}
	fixture.invite, err = enrollment.IssueInvitation(discoveryPort)
	if err != nil {
		t.Fatal(err)
	}
	fixture.startServers(t, discoveryListener, secureListener, serverCertificate, discoveryInfo, handler)
	t.Cleanup(func() {
		fixture.stopServers()
	})
	return fixture
}

func (f *pairingFixture) startServers(t *testing.T, discoveryListener, secureListener net.Listener, certificate tls.Certificate, discoveryInfo storage.SecurityDiscovery, handler http.Handler) {
	t.Helper()
	secureMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/enrollment/requests" {
			f.enrollmentPosts.Add(1)
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/enrollment/requests/") {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			if recorder.Code >= 200 && recorder.Code < 300 {
				f.receiptSuccesses.Add(1)
			}
			copyPairingRecorder(w, recorder)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/enrollment/requests" && f.losePost.Swap(false) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			if recorder.Code >= 200 && recorder.Code < 300 {
				http.Error(w, "synthetic lost response", http.StatusServiceUnavailable)
				return
			}
			copyPairingRecorder(w, recorder)
			return
		}
		handler.ServeHTTP(w, r)
	})
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(mustRootBlock(t, discoveryInfo.RootPEM))
	f.secure = &http.Server{Handler: secureMux}
	secureConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, ClientCAs: clientRoots, ClientAuth: tls.VerifyClientCertIfGiven}
	go f.secure.Serve(tls.NewListener(secureListener, secureConfig))
	discoveryHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/discovery" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(discoveryInfo)
	})
	f.discovery = &http.Server{Handler: discoveryHandler}
	go f.discovery.Serve(discoveryListener)
}

func copyPairingRecorder(w http.ResponseWriter, recorder *httptest.ResponseRecorder) {
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func (f *pairingFixture) stopServers() {
	if f.secure == nil {
		return
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.secure.Shutdown(shutdown)
	_ = f.discovery.Shutdown(shutdown)
	if streams, ok := f.handler.(interface{ ShutdownStreams(context.Context) error }); ok {
		_ = streams.ShutdownStreams(shutdown)
	}
	if f.store != nil {
		_ = f.store.Close()
		f.store = nil
	}
	if f.enrollment != nil {
		_ = f.enrollment.Close()
		f.enrollment = nil
	}
	f.secure = nil
	f.discovery = nil
}

func (f *pairingFixture) restartServers(t *testing.T) {
	t.Helper()
	f.stopServers()
	discoveryListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(f.discoveryPort)))
	if err != nil {
		t.Fatal(err)
	}
	secureListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(f.securePort)))
	if err != nil {
		discoveryListener.Close()
		t.Fatal(err)
	}
	ctx := context.Background()
	f.enrollment, err = storage.OpenSecurityEnrollment(ctx, f.profileDir)
	if err != nil {
		discoveryListener.Close()
		secureListener.Close()
		t.Fatal(err)
	}
	localURL := fmt.Sprintf("https://127.0.0.1:%d", f.securePort)
	f.admin, err = f.enrollment.ProvisionLocal(ctx, localURL, f.admin.InstanceID)
	if err != nil {
		f.enrollment.Close()
		discoveryListener.Close()
		secureListener.Close()
		t.Fatal(err)
	}
	discoveryInfo := f.enrollment.Discovery(f.securePort)
	certificate, err := storage.EnsureProfileServerTLS(f.profileDir, discoveryInfo.ClusterID, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(ctx, f.storePath)
	if err != nil {
		t.Fatal(err)
	}
	config := server.HandlerConfig{
		SecurityEnrollment: f.enrollment, LocalNodeID: f.admin.NodeID, DiscoveryPort: f.discoveryPort,
		ProcessNonce: uuid.NewString(), SecureBind: secureListener.Addr().String(), Shutdown: func() {},
	}
	f.handler, err = server.NewHandlerWithConfig(f.store, "fixture-only-key", config)
	if err != nil {
		t.Fatal(err)
	}
	f.startServers(t, discoveryListener, secureListener, certificate, discoveryInfo, f.handler)
}

func mustRootBlock(t *testing.T, rootPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(rootPEM))
	if block == nil {
		t.Fatal("missing synthetic root")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPairRemotePinnedDiscoveryPublishesActivatedProfile(t *testing.T) {
	fixture := newPairingFixture(t)
	address := fmt.Sprintf("https://127.0.0.1:%d", fixture.securePort)
	paired, err := storage.PairRemote(context.Background(), fixture.stateDir, address, fixture.invite.Invitation)
	if err != nil {
		t.Fatal(err)
	}
	canonicalState, err := filepath.EvalSymlinks(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if paired.ProfilePath != filepath.Join(canonicalState, "peers", paired.Profile.ClusterID, "client-profile.json") {
		t.Fatalf("profile path = %q", paired.ProfilePath)
	}
	if paired.Profile.URL != address || paired.Profile.ClusterID == "" || paired.Profile.NodeID == "" || paired.Profile.InstanceID == "" || paired.Profile.Serial == "" {
		t.Fatalf("incomplete peer profile: %+v", paired.Profile)
	}
	if _, err := os.Stat(paired.ProfilePath); err != nil {
		t.Fatalf("published profile missing: %v", err)
	}
	for _, path := range []string{paired.Profile.RootPEMPath, paired.Profile.CertificatePEMPath, paired.Profile.PrivateKeyPEMPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("profile credential missing at %q: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(paired.ProfilePath), "root-key.pem")); !os.IsNotExist(err) {
		t.Fatalf("peer profile contains issuer key: stat error=%v", err)
	}
	if _, err := fixture.enrollment.AuthorizeCertificate(context.Background(), mustCertificate(t, paired.Profile.CertificatePEMPath)); err != nil {
		t.Fatalf("published certificate is not active: %v", err)
	}
	credentials, err := storage.LoadPeerClientCredentials(paired.ProfilePath)
	if err != nil {
		t.Fatalf("load published peer profile: %v", err)
	}
	if credentials.Profile.NodeID != paired.Profile.NodeID || credentials.Certificate.Leaf == nil || credentials.RootCAs == nil {
		t.Fatalf("loaded peer credentials are incomplete: %+v", credentials.Profile)
	}
}

func mustCertificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatal("invalid published certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestPairRemoteRecoversIssuedReceiptAfterLostPostResponse(t *testing.T) {
	fixture := newPairingFixture(t)
	fixture.losePost.Store(true)
	address := fmt.Sprintf("https://127.0.0.1:%d", fixture.securePort)
	if _, err := storage.PairRemote(context.Background(), fixture.stateDir, address, fixture.invite.Invitation); err == nil {
		t.Fatal("lost issuance response was reported as completed")
	}
	pendingPath := filepath.Join(fixture.stateDir, "peers", mustInvitationCluster(t, fixture.invite.Invitation), "pair-pending.json")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("durable pending state missing after lost response: %v", err)
	}
	paired, err := storage.PairRemote(context.Background(), fixture.stateDir, address, fixture.invite.Invitation)
	if err != nil {
		t.Fatal(err)
	}
	if paired.Profile.NodeID == "" {
		t.Fatal("receipt recovery did not publish the issued profile")
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("successful activation did not clear pending state: %v", err)
	}
}

func TestPairRemoteRecoversReceiptAfterFullServerRestartAndBypassesProxy(t *testing.T) {
	fixture := newPairingFixture(t)
	proxyRequests, proxyURL := startRejectingProxy(t)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, proxyURL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	address := fmt.Sprintf("https://127.0.0.1:%d", fixture.securePort)
	cluster := mustInvitationCluster(t, fixture.invite.Invitation)
	canonicalState, err := filepath.EvalSymlinks(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := fixture.enrollment.Discovery(fixture.securePort).RootPEM
	fixture.losePost.Store(true)
	if _, err := storage.PairRemote(context.Background(), fixture.stateDir, address, fixture.invite.Invitation); err == nil {
		t.Fatal("lost issuance response was reported as completed")
	}
	if got := fixture.enrollmentPosts.Load(); got != 1 {
		t.Fatalf("initial enrollment POST count = %d, want 1", got)
	}
	if got := fixture.receiptSuccesses.Load(); got != 0 {
		t.Fatalf("unexpected successful receipt before restart: %d", got)
	}
	requestID := pendingPairingRequestID(t, canonicalState, cluster)
	fixture.restartServers(t)
	if fixture.enrollment.Discovery(fixture.securePort).RootPEM != rootBefore {
		t.Fatal("server restart changed the pinned CA")
	}
	if fixture.discoveryPort == 0 || fixture.securePort == 0 {
		t.Fatal("server restart lost its configured ports")
	}
	if _, err := os.Stat(filepath.Join(fixture.profileDir, "security.sqlite")); err != nil {
		t.Fatalf("server security storage was not retained: %v", err)
	}
	if _, err := storage.PairRemote(context.Background(), fixture.stateDir, address, fixture.invite.Invitation); err != nil {
		t.Fatalf("pairing did not recover its receipt after server restart: %v", err)
	}
	if got := fixture.enrollmentPosts.Load(); got != 1 {
		t.Fatalf("restart retry issued another enrollment POST: count=%d", got)
	}
	if got := fixture.receiptSuccesses.Load(); got != 1 {
		t.Fatalf("successful enrollment receipt reads = %d, want 1", got)
	}
	if got := proxyRequests.Load(); got != 0 {
		t.Fatalf("pairing contacted rejecting proxy %d times", got)
	}
	pairedPath := filepath.Join(canonicalState, "peers", cluster, "client-profile.json")
	credentials, err := storage.LoadPeerClientCredentials(pairedPath)
	if err != nil {
		t.Fatalf("load profile recovered from receipt %s: %v", requestID, err)
	}
	if _, err := fixture.enrollment.AuthorizeCertificate(context.Background(), credentials.Certificate.Leaf); err != nil {
		t.Fatalf("recovered profile was not activated: %v", err)
	}
	newState := t.TempDir()
	_, err = storage.PairRemote(context.Background(), newState, address, fixture.invite.Invitation)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "No active peering key") ||
		strings.Contains(err.Error(), fixture.invite.Invitation) {
		t.Fatal("restart rejection did not provide safe peering key recovery guidance")
	}
	fresh, err := fixture.enrollment.IssueInvitation(fixture.discoveryPort)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.PairRemote(context.Background(), newState, address, fresh.Invitation); err != nil {
		t.Fatalf("fresh key failed to recover the pending client state: %v", err)
	}
	if got := proxyRequests.Load(); got != 0 {
		t.Fatalf("recovery contacted rejecting proxy %d times", got)
	}
}

func startRejectingProxy(t *testing.T) (*atomic.Int32, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requests := &atomic.Int32{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return
			}
			requests.Add(1)
			_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = listener.Close()
		<-done
	})
	return requests, "http://" + listener.Addr().String()
}

func pendingPairingRequestID(t *testing.T, stateDir, cluster string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "peers", cluster, "pair-pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &pending); err != nil || pending.RequestID == "" {
		t.Fatalf("invalid durable pairing receipt key: %v", err)
	}
	return pending.RequestID
}

func TestPairRemoteRejectsInvitationPinMismatchBeforeWritingPeerState(t *testing.T) {
	fixture := newPairingFixture(t)
	bad := []byte(fixture.invite.Invitation)
	if bad[8] == '0' {
		bad[8] = '1'
	} else {
		bad[8] = '0'
	}
	state := t.TempDir()
	address := fmt.Sprintf("https://127.0.0.1:%d", fixture.securePort)
	if _, err := storage.PairRemote(context.Background(), state, address, string(bad)); err == nil {
		t.Fatal("accepted a peer with a mismatched cluster pin")
	}
	if _, err := os.Stat(filepath.Join(state, "peers")); !os.IsNotExist(err) {
		t.Fatalf("pin failure wrote peer state: stat error=%v", err)
	}
}

func mustInvitationCluster(t *testing.T, invitation string) string {
	t.Helper()
	parts := strings.Split(invitation, ".")
	if len(parts) < 2 {
		t.Fatal("invalid fixture invitation")
	}
	return parts[1]
}
