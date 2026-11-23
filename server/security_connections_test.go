package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestSecureConnectionsTrackVerifiedLeafUntilSocketClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := t.TempDir()
	enrollment, err := storage.OpenSecurityEnrollment(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer enrollment.Close()
	invite, err := enrollment.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	peer := newTLSFixtureIdentity(t, enrollment, invite.Invitation)
	serverLeaf, err := storage.EnsureProfileServerTLS(profile, peer.response.ClusterID, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(peer.response.RootPEM)) {
		t.Fatal("could not load profile root")
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, tracker, err := TrackSecureConnections(raw, &tls.Config{
		Certificates: []tls.Certificate{serverLeaf}, ClientCAs: roots,
		ClientAuth: tls.VerifyClientCertIfGiven, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer listener.Close()
	serverSocket := make(chan *tls.Conn, 1)
	handshakeDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			socket := conn.(*tls.Conn)
			serverSocket <- socket
			err = socket.HandshakeContext(ctx)
		}
		handshakeDone <- err
	}()
	dialer := tls.Dialer{Config: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{peer.cert}, MinVersion: tls.VersionTLS12}}
	client, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := <-handshakeDone; err != nil {
		t.Fatal(err)
	}
	socket := <-serverSocket
	defer socket.Close()
	if tracker.Snapshot()[peer.response.Serial] != 1 {
		t.Fatal("verified client serial was not leased")
	}
	serial, err := storage.SecurityCertificateSerial(serverLeaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	observeSecureHandshake(socket, http.StateActive)
	if tracker.serverActive[serial] != 1 || tracker.serverSeen != serial {
		t.Fatal("server credential was not reserved and observed")
	}
	presented := socket.ConnectionState().PeerCertificates[0]
	renewed, err := enrollment.RenewCertificate(ctx, presented, storage.RenewalRequest{
		RequestID: uuid.NewString(), CSRPEM: peer.request.CSRPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(renewed.CertificatePEM))
	pending, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enrollment.ActivateCertificate(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := tracker.RetireCredentials(ctx, enrollment, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := enrollment.AuthorizeCertificate(ctx, presented); err != nil {
		t.Fatalf("old credential retired with a live connection: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(profile, "server-identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var original struct {
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(manifest, &original); err != nil {
		t.Fatal(err)
	}
	names := []string{"127.0.0.1", "localhost"}
	if err := tracker.RefreshServerCertificate(enrollment, profile, peer.response.ClusterID, names, time.Now()); err != nil {
		t.Fatal(err)
	}
	oldContext := secureSocketContext(ctx, socket)
	if !validServerGeneration(oldContext, time.Now()) || validServerGeneration(oldContext, time.UnixMilli(tracker.previousUntil)) {
		t.Fatal("previous server generation did not enforce its exact overlap deadline")
	}
	httpCtx, stopHTTP := context.WithCancel(ctx)
	httpDone := make(chan error, 1)
	go func() {
		httpDone <- Serve(httpCtx, listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}), time.Second)
	}()
	defer func() {
		stopHTTP()
		if err := <-httpDone; err != nil {
			t.Error(err)
		}
	}()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	nextSerial, err := storage.SecurityCertificateSerial(response.TLS.PeerCertificates[0].Raw)
	if err != nil || nextSerial == serial || response.StatusCode != http.StatusNoContent {
		t.Fatalf("existing listener did not present the rotated leaf: serial=%s status=%d err=%v", nextSerial, response.StatusCode, err)
	}
	if err := tracker.RefreshServerCertificate(enrollment, profile, peer.response.ClusterID, names, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(original.PrivateKey); err != nil {
		t.Fatalf("old key was removed before its socket closed: %v", err)
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	if len(tracker.Snapshot()) != 0 {
		t.Fatal("closed connection retained its credential lease")
	}
	if err := tracker.RefreshServerCertificate(enrollment, profile, peer.response.ClusterID, names, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(original.PrivateKey); !os.IsNotExist(err) {
		t.Fatalf("drained server key remained on disk: %v", err)
	}
	if err := tracker.RefreshServerCertificate(enrollment, profile, peer.response.ClusterID, []string{"127.0.0.1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	status, err := enrollment.ServerIdentityStatus(ctx)
	if err != nil || status.Serial == nextSerial || len(status.Names) != 1 || status.Names[0] != "127.0.0.1" {
		t.Fatalf("removing a SAN did not rotate the certificate: %+v %v", status, err)
	}
	if err := tracker.RetireCredentials(ctx, enrollment, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := enrollment.AuthorizeCertificate(ctx, presented); err == nil {
		t.Fatal("drained credential remained authorized")
	}
}
