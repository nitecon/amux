package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestServerCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		{}, {"desktop"}, {"server"}, {"server", "run"}, {"server", "run", "--database", filepath.Join(t.TempDir(), "db"), "--profile", t.TempDir()}, {"server", "run", "--database", filepath.Join(t.TempDir(), "db"), "--listen", "0.0.0.0:0"},
		{"server", "run", "--database", filepath.Join(t.TempDir(), "db"), "--profile-tls-name", "localhost"},
		{"server", "run", "--profile", t.TempDir(), "--profile-tls-name", "localhost", "--tls-cert", "manual.pem", "--tls-key", "manual-key.pem"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out, &out, profileTestEnv); err == nil {
			t.Fatalf("invalid command accepted: %q", args)
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"server", "run", "--database", filepath.Join(t.TempDir(), "db")}, &out, &out, func(string) string { return "" }); err == nil {
		t.Fatal("missing key accepted")
	}
	if err := run(context.Background(), []string{"server", "run", "--help"}, &out, &out, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
}

func TestServerProfileStartupAndRestartPreserveInstallationIdentity(t *testing.T) {
	profile := t.TempDir()
	_, stopFirst := startProfileServer(t, profile)
	if lock, err := server.LockSecurityProfile(profile); err == nil {
		lock.Close()
		t.Fatal("security profile lock was not held during startup")
	}
	if lock, err := server.LockProfile(filepath.Join(profile, "application.sqlite")); err == nil {
		lock.Close()
		t.Fatal("application profile lock was not held during startup")
	}
	stopFirst()
	first := readProfileClusterID(t, filepath.Join(profile, "security.sqlite"))
	firstRoot := readProfileFile(t, filepath.Join(profile, "root.pem"))
	firstKey := readProfileFile(t, filepath.Join(profile, "root-key.pem"))
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("cluster identity=%q: %v", first, err)
	}
	if _, err := os.Stat(filepath.Join(profile, "application.sqlite")); err != nil {
		t.Fatalf("application database missing: %v", err)
	}
	_, stopSecond := startProfileServer(t, profile)
	stopSecond()
	second := readProfileClusterID(t, filepath.Join(profile, "security.sqlite"))
	if second != first {
		t.Fatalf("cluster identity changed across startup: %q -> %q", first, second)
	}
	if !bytes.Equal(firstRoot, readProfileFile(t, filepath.Join(profile, "root.pem"))) || !bytes.Equal(firstKey, readProfileFile(t, filepath.Join(profile, "root-key.pem"))) {
		t.Fatal("profile root material changed across restart")
	}
	appLock, err := server.LockProfile(filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatalf("application lock not released: %v", err)
	}
	appLock.Close()
	securityLock, err := server.LockSecurityProfile(profile)
	if err != nil {
		t.Fatalf("security lock not released: %v", err)
	}
	securityLock.Close()
}

func TestServerProfileStartupReusesSyntheticRustInstallationRow(t *testing.T) {
	profile := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(profile, "security.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE installation(id INTEGER PRIMARY KEY CHECK(id=1),cluster_id TEXT NOT NULL);
		INSERT INTO installation(id,cluster_id) VALUES(1,'rust-fixture-cluster');`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, stop := startProfileServer(t, profile)
	stop()
	if got := readProfileClusterID(t, filepath.Join(profile, "security.sqlite")); got != "rust-fixture-cluster" {
		t.Fatalf("startup replaced existing Rust identity with %q", got)
	}
}

func TestLegacyDatabaseModeDoesNotInferSecurityProfile(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "arbitrary.sqlite")
	_, stop := startServerCommand(t, "--database", databasePath, "--listen", "127.0.0.1:0")
	stop()
	for _, name := range []string{"security.sqlite", ".security-owner.lock"} {
		if _, err := os.Stat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy database mode inferred %s: stat err=%v", name, err)
		}
	}
}

func TestProfileTLSListenerServesPersistentChainAndSANs(t *testing.T) {
	profile := t.TempDir()
	args := []string{"--profile", profile, "--profile-tls-name", "localhost", "--profile-tls-name", "127.0.0.1", "--listen", "127.0.0.1:0"}
	address, stop := startServerCommand(t, args...)
	rootPEM := readProfileFile(t, filepath.Join(profile, "root.pem"))
	rootBlock, _ := pem.Decode(rootPEM)
	if rootBlock == nil {
		t.Fatal("profile root certificate missing")
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	var firstLeaf []byte
	for _, name := range []string{"localhost", "127.0.0.1"} {
		if unauthenticated := requestProfileTLS(t, address, name, pool, false); unauthenticated == nil {
			t.Fatalf("unauthenticated TLS request did not complete for %q", name)
		}
		leaf := requestProfileTLS(t, address, name, pool, true)
		if err := leaf.VerifyHostname(name); err != nil {
			t.Fatalf("leaf SAN does not match %q: %v", name, err)
		}
		if firstLeaf == nil {
			firstLeaf = append([]byte(nil), leaf.Raw...)
		}
	}
	stop()
	restartedAddress, stop := startServerCommand(t, args...)
	restartedLeaf := requestProfileTLS(t, restartedAddress, "localhost", pool, true)
	if !bytes.Equal(firstLeaf, restartedLeaf.Raw) {
		t.Fatal("TLS leaf changed on a restart with unchanged profile names")
	}
	if wrongNameLeaf := requestProfileTLS(t, restartedAddress, "wrong.example", pool, false); wrongNameLeaf != nil {
		t.Fatal("TLS handshake accepted a name outside the configured SANs")
	}
	stop()
	if openssl, err := exec.LookPath("openssl"); err == nil {
		leafPath := filepath.Join(t.TempDir(), "leaf.pem")
		leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: firstLeaf})
		if err := os.WriteFile(leafPath, leafPEM, 0600); err != nil {
			t.Fatal(err)
		}
		rootPath := filepath.Join(profile, "root.pem")
		output, err := exec.Command(openssl, "verify", "-x509_strict", "-CAfile", rootPath, "-verify_hostname", "localhost", leafPath).CombinedOutput()
		if err != nil {
			t.Fatalf("OpenSSL strict verification failed: %v: %s", err, output)
		}
	}
}

func TestSecureProfileListenerRequiresEnrolledCertificate(t *testing.T) {
	profile := t.TempDir()
	instance := uuid.NewString()
	address, stop := startServerCommand(t, "--profile", profile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", instance, "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stop()
	var local struct {
		InstanceID  string `json:"instance_id"`
		URL         string `json:"url"`
		Certificate string `json:"certificate_pem_path"`
		Key         string `json:"private_key_pem_path"`
	}
	if err := json.Unmarshal(readProfileFile(t, filepath.Join(profile, "local-client.json")), &local); err != nil {
		t.Fatal(err)
	}
	if local.InstanceID != instance || local.URL != "https://"+address {
		t.Fatalf("incorrect local endpoint identity: %+v", local)
	}
	certificate, err := tls.LoadX509KeyPair(local.Certificate, local.Key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(readProfileFile(t, filepath.Join(profile, "root.pem"))) {
		t.Fatal("invalid profile root")
	}
	for _, test := range []struct {
		name         string
		certificates []tls.Certificate
		bearer       string
		want         int
	}{
		{name: "bearer alone", bearer: "profile-test-key", want: http.StatusUnauthorized},
		{name: "enrolled administrator", certificates: []tls.Certificate{certificate}, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: test.certificates, MinVersion: tls.VersionTLS12}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			request, err := http.NewRequest(http.MethodGet, local.URL+"/v1/projects", nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.bearer != "" {
				request.Header.Set("Authorization", "Bearer "+test.bearer)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("got %d, want %d", response.StatusCode, test.want)
			}
			if response.StatusCode == http.StatusOK {
				verifySecureDiscovery(t, client, local.URL, address, readProfileFile(t, filepath.Join(profile, "root.pem")))
			}
		})
	}
}

func TestSecureStatusAndNonceFencedShutdown(t *testing.T) {
	profile := t.TempDir()
	address, stop := startServerCommand(t, "--profile", profile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", uuid.NewString(), "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stop()
	var local struct {
		URL         string `json:"url"`
		Certificate string `json:"certificate_pem_path"`
		Key         string `json:"private_key_pem_path"`
	}
	if err := json.Unmarshal(readProfileFile(t, filepath.Join(profile, "local-client.json")), &local); err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.LoadX509KeyPair(local.Certificate, local.Key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(readProfileFile(t, filepath.Join(profile, "root.pem"))) {
		t.Fatal("invalid profile trust root")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{certificate}}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	type status struct {
		Running      bool   `json:"running"`
		ProcessNonce string `json:"process_nonce"`
		SecureBind   string `json:"secure_bind"`
		Nodes        []struct {
			Role string `json:"role"`
		} `json:"nodes"`
		ServerSerial string `json:"server_serial"`
	}
	getStatus := func() status {
		t.Helper()
		response, err := client.Get(local.URL + "/v1/server/status")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var value status
		if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&value) != nil {
			t.Fatalf("status response %d", response.StatusCode)
		}
		return value
	}
	current := getStatus()
	if !current.Running || current.ProcessNonce == "" || current.SecureBind != address || current.ServerSerial == "" {
		t.Fatalf("incomplete process status: %+v", current)
	}
	foundAdmin := false
	for _, node := range current.Nodes {
		foundAdmin = foundAdmin || node.Role == "administrator"
	}
	if !foundAdmin {
		t.Fatalf("status omitted local administrator node: %+v", current.Nodes)
	}
	postShutdown := func(nonce string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"expected_process_nonce": nonce})
		response, err := client.Post(local.URL+"/v1/server/shutdown", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	stale := postShutdown("stale-process-nonce")
	_ = stale.Body.Close()
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale shutdown returned %d, want 409", stale.StatusCode)
	}
	if got := getStatus(); got.ProcessNonce != current.ProcessNonce {
		t.Fatal("stale shutdown changed the running process")
	}
	streamCtx, cancelStream := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStream()
	streamURL := "wss://" + address + "/v1/tasks/stream"
	conn, _, err := websocket.Dial(streamCtx, streamURL, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test complete")
	if err := wsjson.Write(streamCtx, conn, map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	var subscribed map[string]any
	if err := wsjson.Read(streamCtx, conn, &subscribed); err != nil || subscribed["type"] != "subscribed" {
		t.Fatalf("task stream did not subscribe: frame=%v err=%v", subscribed, err)
	}
	exact := postShutdown(current.ProcessNonce)
	_ = exact.Body.Close()
	if exact.StatusCode != http.StatusAccepted {
		t.Fatalf("exact shutdown returned %d, want 202", exact.StatusCode)
	}
	if _, _, err := conn.Read(streamCtx); err == nil {
		t.Fatal("task stream remained open after shutdown began")
	}
	stop()
	appLock, err := server.LockProfile(filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatalf("application profile lock leaked after shutdown: %v", err)
	}
	appLock.Close()
	securityLock, err := server.LockSecurityProfile(profile)
	if err != nil {
		t.Fatalf("security profile lock leaked after shutdown: %v", err)
	}
	securityLock.Close()
}

func verifySecureDiscovery(t *testing.T, admin *http.Client, secureURL, address string, rootPEM []byte) {
	t.Helper()
	inviteResponse, err := admin.Post(secureURL+"/v1/server/invitation", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer inviteResponse.Body.Close()
	var invitation storage.Invitation
	if inviteResponse.StatusCode != http.StatusOK || json.NewDecoder(inviteResponse.Body).Decode(&invitation) != nil {
		t.Fatalf("generate invitation returned %d or an invalid response", inviteResponse.StatusCode)
	}
	parts := strings.Split(invitation.Invitation, ".")
	if len(parts) != 6 {
		t.Fatal("invalid peering key format")
	}
	host, securePort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	discoveryURL := "http://" + net.JoinHostPort(host, parts[5])
	public := &http.Client{Timeout: 5 * time.Second}
	resp, err := public.Get(discoveryURL + "/v1/discovery")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var discovery storage.SecurityDiscovery
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&discovery) != nil {
		t.Fatalf("discovery returned %d or an invalid response", resp.StatusCode)
	}
	root, _ := pem.Decode(rootPEM)
	hash := sha256.Sum256(root.Bytes)
	if discovery.Version != 1 || discovery.ClusterID != parts[1] || discovery.RootSHA256 != parts[2] ||
		discovery.RootSHA256 != hex.EncodeToString(hash[:]) || discovery.RootPEM != string(rootPEM) ||
		strconv.Itoa(int(discovery.SecurePort)) != securePort {
		t.Fatal("discovery and administrator invitation disagree on pinned trust or listener")
	}
	verifyRemoteEnrollment(t, secureURL, invitation.Invitation, discovery)
	stateDir := t.TempDir()
	var output bytes.Buffer
	if err := runClientEnroll(context.Background(), []string{"--server", secureURL, "--state-dir", stateDir},
		strings.NewReader(invitation.Invitation+"\n"), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	manifest := strings.TrimSpace(output.String())
	if filepath.Base(manifest) != "client-profile.json" {
		t.Fatal("enrollment did not return the credential manifest path")
	}
	var enrolled storage.LocalSecurityProfile
	if err := json.Unmarshal(readProfileFile(t, manifest), &enrolled); err != nil {
		t.Fatal(err)
	}
	if enrolled.ClusterID != discovery.ClusterID || enrolled.NodeID == "" || enrolled.URL != secureURL {
		t.Fatal("persisted client identity does not match the secure gateway")
	}
	peerClient, _, err := gatewayClientForProfile("", manifest, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peerClient.Projects(context.Background()); err != nil {
		t.Fatalf("the persisted peer profile could not access the gateway: %v", err)
	}
	if err := run(context.Background(), []string{"client", "renew", "--enrolled-profile", manifest}, io.Discard, io.Discard, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	renewed, err := storage.LoadPeerClientCredentials(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Profile.NodeID != enrolled.NodeID || renewed.Profile.InstanceID != enrolled.InstanceID || renewed.Profile.Serial == enrolled.Serial {
		t.Fatal("renewal did not rotate the certificate while retaining the enrolled identity")
	}
}

func verifyRemoteEnrollment(t *testing.T, secureURL, invitation string, discovery storage.SecurityDiscovery) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(storage.EnrollmentRequest{
		RequestID: uuid.NewString(), InstanceID: uuid.NewString(), Invitation: invitation,
		CSRPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})),
	})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(discovery.RootPEM)) {
		t.Fatal("discovered root could not be parsed")
	}
	bootstrap := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer bootstrap.CloseIdleConnections()
	client := &http.Client{Transport: bootstrap, Timeout: 5 * time.Second}
	resp, err := client.Post(secureURL+"/v1/enrollment/requests", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var enrolled storage.EnrollmentResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&enrolled) != nil || enrolled.ClusterID != discovery.ClusterID {
		t.Fatalf("enrollment failed or returned another cluster, status %d", resp.StatusCode)
	}
	leaf, _ := pem.Decode([]byte(enrolled.CertificatePEM))
	if leaf == nil {
		t.Fatal("enrollment did not return a certificate")
	}
	peer := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Bytes}, PrivateKey: key}},
	}}
	defer peer.CloseIdleConnections()
	client.Transport = peer
	for _, route := range []struct {
		path string
		want int
	}{
		{"/v1/projects", http.StatusOK},
		{"/v1/server/invitation", http.StatusForbidden},
	} {
		response, err := client.Get(secureURL + route.path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != route.want {
			t.Fatalf("%s returned %d, want %d", route.path, response.StatusCode, route.want)
		}
	}
}

func requestProfileTLS(t *testing.T, address, serverName string, roots *x509.CertPool, authenticated bool) *x509.Certificate {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodGet, "https://"+address+"/v1/projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer profile-test-key")
	}
	response, err := client.Do(request)
	if err != nil {
		if authenticated {
			t.Fatalf("profile TLS request for %q: %v", serverName, err)
		}
		return nil
	}
	defer response.Body.Close()
	wantStatus := http.StatusUnauthorized
	if authenticated {
		wantStatus = http.StatusOK
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("profile TLS status=%d, want %d", response.StatusCode, wantStatus)
	}
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || len(response.TLS.VerifiedChains) == 0 {
		t.Fatal("TLS connection did not expose a verified profile chain")
	}
	return response.TLS.PeerCertificates[0]
}

func TestServerProfileStartupFailureReleasesBothLocks(t *testing.T) {
	profile := t.TempDir()
	if err := os.Mkdir(filepath.Join(profile, "security.sqlite"), 0700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := run(context.Background(), []string{"server", "run", "--profile", profile}, &out, &out, profileTestEnv)
	if err == nil {
		t.Fatal("startup accepted a directory as security.sqlite")
	}
	appLock, err := server.LockProfile(filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatalf("application lock leaked after startup failure: %v", err)
	}
	appLock.Close()
	securityLock, err := server.LockSecurityProfile(profile)
	if err != nil {
		t.Fatalf("security lock leaked after startup failure: %v", err)
	}
	securityLock.Close()
}

func startProfileServer(t *testing.T, profile string) (string, func()) {
	t.Helper()
	return startServerCommand(t, "--profile", profile, "--listen", "127.0.0.1:0")
}

func startServerCommand(t *testing.T, serverArgs ...string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		args := append([]string{"server", "run"}, serverArgs...)
		err := run(ctx, args, writer, io.Discard, profileTestEnv)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "listening ") {
		cancel()
		select {
		case runErr := <-done:
			t.Fatalf("profile server did not start: line=%q read=%v run=%v", line, err, runErr)
		case <-time.After(10 * time.Second):
			t.Fatal("profile server startup did not terminate")
		}
	}
	address := strings.TrimSpace(strings.TrimPrefix(line, "listening "))
	var stopOnce sync.Once
	return address, func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("profile server shutdown: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("profile server did not stop")
			}
			reader.Close()
		})
	}
}

func profileTestEnv(name string) string {
	if name == "GATEWAY_API_KEY" {
		return "profile-test-key"
	}
	return ""
}

func readProfileClusterID(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow("SELECT cluster_id FROM installation WHERE id=1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func readProfileFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestServerTickLoopPublishesAndStops(t *testing.T) {
	bus := eventbus.New()
	sub := bus.Subscribe(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		server.RunEventTicks(ctx, bus, 5*time.Millisecond)
		close(done)
	}()
	select {
	case event, ok := <-sub.C:
		if !ok || event.Topic != eventbus.Tick {
			t.Fatalf("tick event = %+v, open=%v", event, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("tick loop did not publish")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick loop did not stop")
	}
	sub.Unsubscribe()
}

func TestServerProcessHelper(t *testing.T) {
	if os.Getenv("AMUX_GO_SERVER_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"amux"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	os.Exit(2)
}

func TestServerRunsWithoutDisplayAndStopsOnSignal(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows shutdown uses service control rather than POSIX interrupt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerProcessHelper$", "--", "server", "run", "--database", filepath.Join(t.TempDir(), "server.db"), "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "AMUX_GO_SERVER_TEST=1", "GATEWAY_API_KEY=process-test-key", "DISPLAY=", "WAYLAND_DISPLAY=", "DBUS_SESSION_BUS_ADDRESS=", "HOME="+t.TempDir())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
		} else {
			ready <- ""
		}
		io.Copy(io.Discard, stdout)
	}()
	var line string
	select {
	case line = <-ready:
	case <-ctx.Done():
		t.Fatal("server did not report readiness")
	}
	address, ok := strings.CutPrefix(line, "listening ")
	if !ok {
		t.Fatalf("server startup failed: %q", line)
	}
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequest("POST", "http://"+address+"/v1/projects/missing/archive", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer process-test-key")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("server response %d", res.StatusCode)
	}
	stream, _, err := websocket.Dial(ctx, "ws://"+address+"/v1/events/stream", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer process-test-key"}}})
	if err != nil {
		t.Fatalf("production router stream: %v", err)
	}
	defer stream.CloseNow()
	if err := wsjson.Write(ctx, stream, map[string]any{"type": "subscribe", "consumer_id": uuid.NewString(), "events": []string{"task.created"}}); err != nil {
		t.Fatal(err)
	}
	var subscribed struct {
		Type                   string `json:"type"`
		ServerRestartReminders bool   `json:"server_restart_reminders"`
	}
	if err := wsjson.Read(ctx, stream, &subscribed); err != nil || subscribed.Type != "subscribed" || !subscribed.ServerRestartReminders {
		t.Fatalf("full builtin attachment: %+v %v", subscribed, err)
	}
	if err := wsjson.Write(ctx, stream, map[string]any{"type": "emit", "id": "client.heartbeat", "version": 1, "payload": map[string]any{"agents": []any{}}}); err != nil {
		t.Fatal(err)
	}
	var emitted struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	}
	if err := wsjson.Read(ctx, stream, &emitted); err != nil || emitted.Type != "emitted" || emitted.UID == "" {
		t.Fatalf("production remote emit: %+v %v", emitted, err)
	}
	if err := wsjson.Write(ctx, stream, map[string]any{"type": "emit", "id": "clock.tick", "version": 1, "payload": json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var forbidden struct {
		Type string `json:"type"`
	}
	if err := wsjson.Read(ctx, stream, &forbidden); err != nil || forbidden.Type != "error" {
		t.Fatalf("remote clock bypass: %+v %v", forbidden, err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("server shutdown: %v: %s", err, stderr.String())
	}
}
