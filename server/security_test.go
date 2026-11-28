package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/storage"
)

type tlsFixtureIdentity struct {
	response storage.EnrollmentResponse
	request  storage.EnrollmentRequest
	cert     tls.Certificate
}

func newTLSFixtureIdentity(t *testing.T, enrollment *storage.SecurityEnrollment, invitation string) tlsFixtureIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "server TLS fixture"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	request := storage.EnrollmentRequest{RequestID: uuid.NewString(), Invitation: invitation, CSRPEM: csr, InstanceID: uuid.NewString()}
	response, err := enrollment.Enroll(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey})
	cert, err := tls.X509KeyPair([]byte(response.CertificatePEM), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return tlsFixtureIdentity{response: response, request: request, cert: cert}
}

func startSecureHTTPFixture(t *testing.T, handler http.Handler, rootPEM string) *httptest.Server {
	return startSecureHTTPFixtureFrom(t, handler, rootPEM, "")
}

func startSecureHTTPFixtureFrom(t *testing.T, handler http.Handler, rootPEM, remoteAddr string) *httptest.Server {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(rootPEM)) {
		t.Fatal("could not load client CA into TLS server")
	}
	if remoteAddr != "" {
		base := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = remoteAddr
			base.ServeHTTP(w, r)
		})
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func secureHTTPClient(t *testing.T, srv *httptest.Server, cert *tls.Certificate) *http.Client {
	t.Helper()
	base, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("unexpected httptest transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	if cert != nil {
		transport.TLSClientConfig.Certificates = []tls.Certificate{*cert}
	}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

func secureFixture(t *testing.T, remote bool) (*storage.Store, *storage.SecurityEnrollment, string, *tlsFixtureIdentity, *tlsFixtureIdentity, *httptest.Server, *http.Client, *http.Client) {
	return secureFixtureFrom(t, remote, "")
}

func secureFixtureFrom(t *testing.T, remote bool, remoteAddr string) (*storage.Store, *storage.SecurityEnrollment, string, *tlsFixtureIdentity, *tlsFixtureIdentity, *httptest.Server, *http.Client, *http.Client) {
	t.Helper()
	ctx := context.Background()
	profile := t.TempDir()
	enrollment, err := storage.OpenSecurityEnrollment(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enrollment.Close() })
	invitation, err := enrollment.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	first := newTLSFixtureIdentity(t, enrollment, invitation.Invitation)
	second := newTLSFixtureIdentity(t, enrollment, invitation.Invitation)
	if _, err := storage.EnsureProfileServerTLS(profile, first.response.ClusterID, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "secure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	config := HandlerConfig{
		SecurityEnrollment: enrollment, LocalNodeID: first.response.NodeID, DiscoveryPort: 24181,
		ProcessNonce: uuid.NewString(), SecureBind: "127.0.0.1:0", DiscoveryBind: "127.0.0.1:24181",
		Shutdown: func() {},
	}
	if remote {
		registry, registryErr := fism.NewRegistry(controllers.NotificationEventDefinitions(), []fism.ControllerDefinition{controllers.RemoteController([]string{controllers.NotifyEventID, controllers.GlobalNotifyEventID}).Definition})
		if registryErr != nil {
			t.Fatal(registryErr)
		}
		config.Remote = &RemoteConfig{Context: ctx, Registry: registry, Forwarded: []string{controllers.NotifyEventID, controllers.GlobalNotifyEventID}}
	}
	handler, err := NewHandlerWithConfig(store, "secure-test-key", config)
	if err != nil {
		t.Fatal(err)
	}
	srv := startSecureHTTPFixtureFrom(t, handler, first.response.RootPEM, remoteAddr)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(cleanup)
	})
	return store, enrollment, profile, &first, &second, srv, secureHTTPClient(t, srv, &first.cert), secureHTTPClient(t, srv, &second.cert)
}

func TestSecureHandlerTLSBootstrapAndPrincipalChecks(t *testing.T) {
	_, enrollment, profile, first, second, srv, firstClient, secondClient := secureFixture(t, false)

	// Bootstrap remains reachable without a client certificate on the two exact routes.
	noCert := secureHTTPClient(t, srv, nil)
	malformed, err := noCert.Post(srv.URL+"/v1/enrollment/requests", "application/json", strings.NewReader("null"))
	if err != nil {
		t.Fatal(err)
	}
	malformed.Body.Close()
	if malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("null enrollment request status=%d, want 400", malformed.StatusCode)
	}
	bootstrapKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "bootstrap client"}}, bootstrapKey)
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := enrollment.CopyInvitation()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapRequest := storage.EnrollmentRequest{RequestID: uuid.NewString(), Invitation: invitation.Invitation, CSRPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})), InstanceID: uuid.NewString()}
	body, _ := json.Marshal(bootstrapRequest)
	response, err := noCert.Post(srv.URL+"/v1/enrollment/requests", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var issued storage.EnrollmentResponse
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("bootstrap POST status %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&issued); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = noCert.Get(srv.URL + "/v1/enrollment/requests/" + bootstrapRequest.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt storage.EnrollmentResponse
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&receipt) != nil || receipt.Serial != issued.Serial {
		response.Body.Close()
		t.Fatalf("bootstrap receipt status=%d receipt=%+v", response.StatusCode, receipt)
	}
	response.Body.Close()
	if response, err = noCert.Get(srv.URL + "/v1/projects"); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("certificate-free API status %d, want 401", response.StatusCode)
	}
	response.Body.Close()

	// Bound a requested runtime instance to its authenticated node; a second
	// certificate cannot take over that identifier.
	boundInstance := uuid.NewString()
	request, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/projects", nil)
	request.Header.Set("X-Agent-Instance-Id", boundInstance)
	response, err = firstClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("first exact instance binding status %d", response.StatusCode)
	}
	response.Body.Close()
	request, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/projects", nil)
	request.Header.Add("X-Agent-Instance-Id", uuid.NewString())
	request.Header.Add("X-Agent-Instance-Id", uuid.NewString())
	response, err = firstClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("duplicate instance header status %d, want 403", response.StatusCode)
	}
	response.Body.Close()
	request, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/projects", nil)
	request.Header.Set("X-Agent-Instance-Id", boundInstance)
	response, err = secondClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-node instance takeover status %d, want 403", response.StatusCode)
	}
	response.Body.Close()

	// Client-role credentials cannot reach management routes, even for the
	// configured local node ID and a loopback peer.
	response, err = firstClient.Get(srv.URL + "/v1/server/status")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("client role management status %d, want 403", response.StatusCode)
	}
	response.Body.Close()

	for _, path := range []string{"/v1/execution/runs", "/v1/users"} {
		response, err = firstClient.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status %d, want %d", path, response.StatusCode, http.StatusForbidden)
		}
	}
	for _, path := range []string{"/v1/task-events", "/v1/task-events/consumers"} {
		response, err = firstClient.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("enrolled client %s status %d, want %d", path, response.StatusCode, http.StatusOK)
		}
	}
	// Promote this synthetic cert to administrator to prove the same gate also
	// requires the exact local node ID and actual socket peer to be loopback.
	db, err := sql.Open("sqlite", filepath.Join(profile, "security.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE nodes SET role='administrator' WHERE id=?", first.response.NodeID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	response, err = firstClient.Get(srv.URL + "/v1/server/status")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("local administrator status endpoint returned %d, want 200", response.StatusCode)
	}
	response.Body.Close()
	for _, path := range []string{"/v1/execution/runs", "/v1/users"} {
		response, err = firstClient.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status %d, want %d", path, response.StatusCode, http.StatusOK)
		}
	}
	response, err = firstClient.Post(srv.URL+"/v1/server/invitation", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var generated storage.Invitation
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&generated) != nil || generated.Invitation == "" {
		response.Body.Close()
		t.Fatalf("local administrator invitation POST status=%d", response.StatusCode)
	}
	response.Body.Close()
	response, err = firstClient.Get(srv.URL + "/v1/server/invitation")
	if err != nil {
		t.Fatal(err)
	}
	var copied storage.Invitation
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&copied) != nil || copied.Invitation != generated.Invitation {
		response.Body.Close()
		t.Fatalf("local administrator invitation GET status=%d", response.StatusCode)
	}
	response.Body.Close()
	request, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/server/invitation", nil)
	response, err = firstClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("local administrator invitation DELETE status %d", response.StatusCode)
	}
	response.Body.Close()

	for _, check := range []struct {
		client *http.Client
		node   string
		want   int
	}{
		{secondClient, second.response.NodeID, http.StatusForbidden},
		{firstClient, first.response.NodeID, http.StatusForbidden},
		{firstClient, second.response.NodeID, http.StatusNoContent},
		{firstClient, second.response.NodeID, http.StatusNoContent},
	} {
		response, err := check.client.Post(srv.URL+"/v1/server/nodes/"+check.node+"/revoke", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != check.want {
			t.Fatalf("revocation status=%d, want %d", response.StatusCode, check.want)
		}
	}
	response, err = secondClient.Get(srv.URL + "/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked certificate status=%d, want 401", response.StatusCode)
	}
}

func TestSecureTaskAndRemoteStreamsBindConsumersToTLSNode(t *testing.T) {
	_, _, _, _, _, srv, firstClient, secondClient := secureFixture(t, true)
	for _, stream := range []struct {
		path       string
		consumerID string
		subscribe  map[string]any
	}{
		{path: "/v1/tasks/stream", consumerID: "shared-task-consumer", subscribe: map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "shared-task-consumer", "after_event_id": 0}},
		{path: "/v1/events/stream", consumerID: uuid.NewString(), subscribe: nil},
	} {
		url := "wss" + strings.TrimPrefix(srv.URL, "https") + stream.path
		connect := func(client *http.Client) *websocket.Conn {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.CloseNow() })
			return conn
		}
		firstConn := connect(firstClient)
		secondConn := connect(secondClient)
		firstSubscribe := stream.subscribe
		secondSubscribe := stream.subscribe
		if stream.path == "/v1/events/stream" {
			firstSubscribe = map[string]any{"type": "subscribe", "consumer_id": stream.consumerID, "events": []string{}, "targets": []string{}}
			secondSubscribe = firstSubscribe
		}
		sendStream(t, firstConn, firstSubscribe)
		if got := frameType(readStream(t, firstConn)); got != "subscribed" {
			t.Fatalf("first node %s subscribe frame type %q", stream.path, got)
		}
		sendStream(t, secondConn, secondSubscribe)
		frame := readStream(t, secondConn)
		if frameType(frame) != "error" || !strings.Contains(string(frame["message"]), "consumer belongs to another node") {
			t.Fatalf("second node took over %s consumer: %s", stream.path, frame)
		}
	}
}

func TestSecureHandlerConfigRequiresEnrollmentAndLocalNodeTogether(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, config := range []HandlerConfig{{SecurityEnrollment: &storage.SecurityEnrollment{}}, {LocalNodeID: uuid.NewString()}} {
		if _, err := NewHandlerWithConfig(store, "key", config); err == nil {
			t.Fatal("accepted partial secure handler configuration")
		}
	}
}

func TestSecureBootstrapLimiterIsBoundedPerPeerAndExpires(t *testing.T) {
	api := newAPI(nil, "unused")
	ip := net.ParseIP("192.0.2.25")
	now := time.Now()
	for i := 0; i < bootstrapLimit; i++ {
		if !api.allowBootstrap(ip, now) {
			t.Fatalf("request %d unexpectedly limited", i+1)
		}
	}
	if api.allowBootstrap(ip, now) {
		t.Fatal("peer exceeded bootstrap rate limit")
	}
	if !api.allowBootstrap(net.ParseIP("192.0.2.26"), now) {
		t.Fatal("rate limit was not scoped to peer IP")
	}
	if !api.allowBootstrap(ip, now.Add(bootstrapWindow)) {
		t.Fatal("expired peer rate limit was retained")
	}
}

func TestSecureManagementRequiresLoopbackEvenForLocalAdministrator(t *testing.T) {
	_, _, profile, first, _, srv, client, _ := secureFixtureFrom(t, false, "192.0.2.10:4444")
	db, err := sql.Open("sqlite", filepath.Join(profile, "security.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE nodes SET role='administrator' WHERE id=?", first.response.NodeID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(srv.URL + "/v1/server/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("non-loopback local administrator status %d, want 403", response.StatusCode)
	}
}
