package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	peerProfileLifetime = 30 * 24 * time.Hour
	peerHTTPBodyLimit   = 64 << 10
)

// PeerProfile identifies the persisted profile returned by PairRemote.
type PeerProfile struct {
	ProfilePath string
	Profile     LocalSecurityProfile
}

type remotePairingPending struct {
	RequestID    string              `json:"request_id"`
	CSRPEM       string              `json:"csr_pem"`
	KeyPath      string              `json:"key_path"`
	RootPath     string              `json:"root_path"`
	InvitationID string              `json:"invitation_id"`
	Response     *EnrollmentResponse `json:"response"`
}

// PairRemote performs invitation-pinned discovery, resumable certificate
// issuance and profile publication. It never reads or creates an issuer key.
func PairRemote(ctx context.Context, stateDir, serverAddress, invitation string) (PeerProfile, error) {
	var empty PeerProfile
	cluster, pin, invitationID, discoveryPort, err := parsePeeringInvitation(strings.TrimSpace(invitation))
	if err != nil {
		return empty, err
	}
	base, err := pairingBaseURL(serverAddress)
	if err != nil {
		return empty, err
	}
	discoveryURL := *base
	discoveryURL.Scheme = "http"
	discoveryURL.Host = net.JoinHostPort(base.Hostname(), strconv.Itoa(int(discoveryPort)))
	discoveryURL.Path = "/v1/discovery"
	discoveryURL.RawPath = ""
	discoveryURL.RawQuery = ""
	discoveryURL.Fragment = ""
	discoveryClient := &http.Client{
		Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	discoveryResponse, err := getPairingResponse(ctx, discoveryClient, http.MethodGet, discoveryURL.String(), nil)
	if err != nil {
		return empty, fmt.Errorf("server discovery unavailable: %w", err)
	}
	defer discoveryClient.CloseIdleConnections()
	var discovery SecurityDiscovery
	if err := decodePairingResponse(discoveryResponse, &discovery); err != nil {
		return empty, fmt.Errorf("server discovery failed: %w", err)
	}
	root, rootDER, err := validatePinnedDiscovery(discovery, cluster, pin, base)
	if err != nil {
		return empty, err
	}

	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return empty, err
	}
	if err := privatePairingDirectory(stateDir); err != nil {
		return empty, err
	}
	stateDir, err = filepath.EvalSymlinks(stateDir)
	if err != nil {
		return empty, err
	}
	instanceID, err := pairingInstallationID(stateDir)
	if err != nil {
		return empty, err
	}
	directory := filepath.Join(stateDir, "peers", cluster)
	if err := privatePairingDirectory(directory); err != nil {
		return empty, err
	}
	lock, err := lockPairingFile(filepath.Join(directory, "pair.lock"))
	if err != nil {
		return empty, fmt.Errorf("pairing is already in progress: %w", err)
	}
	defer lock.Close()

	rootPath := filepath.Join(directory, "root.pem")
	pendingPath := filepath.Join(directory, "pair-pending.json")
	activePath := filepath.Join(directory, "client-profile.json")
	pending, err := loadRemotePairingPending(directory, pendingPath, invitationID, rootPath)
	if err != nil {
		return empty, err
	}
	if pending.Response != nil && pending.Response.ExpiresAt <= time.Now().UnixMilli() {
		if pending.InvitationID == invitationID {
			return empty, fmt.Errorf("pairing certificate expired; use a newly generated peering key")
		}
		pending, err = newRemotePairingPending(directory, invitationID)
		if err != nil {
			return empty, err
		}
	}
	if pending.RequestID == "" {
		pending, err = newRemotePairingPending(directory, invitationID)
		if err != nil {
			return empty, err
		}
	}
	if err := replacePrivateFile(rootPath, []byte(discovery.RootPEM)); err != nil {
		return empty, fmt.Errorf("save pinned server root: %w", err)
	}
	pending.RootPath = rootPath

	client, err := pinnedEnrollmentClient(root, 15*time.Second)
	if err != nil {
		return empty, err
	}
	defer client.CloseIdleConnections()
	if pending.Response == nil {
		receiptURL := pairingEndpoint(base, "v1/enrollment/requests/"+pending.RequestID)
		response, requestErr := getPairingResponse(ctx, client, http.MethodGet, receiptURL, nil)
		if requestErr != nil {
			return empty, fmt.Errorf("enrollment receipt unavailable: %w", requestErr)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			var issued EnrollmentResponse
			if err := decodePairingResponse(response, &issued); err != nil {
				return empty, fmt.Errorf("enrollment receipt failed: %w", err)
			}
			pending.Response = &issued
		} else {
			status := response.StatusCode
			response.Body.Close()
			if status != http.StatusNotFound {
				return empty, fmt.Errorf("enrollment receipt failed (HTTP %d); pending state was retained", status)
			}
			if pending.InvitationID != invitationID {
				pending, err = newRemotePairingPending(directory, invitationID)
				if err != nil {
					return empty, err
				}
				pending.RootPath = rootPath
				if err := replacePrivateFile(rootPath, []byte(discovery.RootPEM)); err != nil {
					return empty, err
				}
			}
			if err := saveRemotePairingPending(pendingPath, pending); err != nil {
				return empty, err
			}
			request := EnrollmentRequest{RequestID: pending.RequestID, Invitation: strings.TrimSpace(invitation), CSRPEM: pending.CSRPEM, InstanceID: instanceID}
			body, err := json.Marshal(request)
			if err != nil {
				return empty, err
			}
			postURL := pairingEndpoint(base, "v1/enrollment/requests")
			response, err = getPairingResponse(ctx, client, http.MethodPost, postURL, bytes.NewReader(body))
			if err != nil {
				return empty, fmt.Errorf("enrollment request unavailable; check the secure port: %w", err)
			}
			var issued EnrollmentResponse
			if err := decodePairingResponse(response, &issued); err != nil {
				return empty, fmt.Errorf("enrollment failed: %w", err)
			}
			pending.Response = &issued
		}
	}
	response := pending.Response
	if response == nil {
		return empty, fmt.Errorf("certificate issuance is unconfirmed")
	}
	if err := validateIssuedPeerCredential(*response, pending, cluster, pin, root, rootDER); err != nil {
		return empty, err
	}
	if err := saveRemotePairingPending(pendingPath, pending); err != nil {
		return empty, err
	}
	profile, err := activateRemotePairing(ctx, base, directory, activePath, instanceID, pending)
	if err != nil {
		return empty, err
	}
	_ = os.Remove(pendingPath)
	return PeerProfile{ProfilePath: activePath, Profile: profile}, nil
}

func parsePeeringInvitation(value string) (cluster, pin, invitationID string, discoveryPort uint16, err error) {
	invalid := func() (string, string, string, uint16, error) {
		return "", "", "", 0, fmt.Errorf("invalid peering key")
	}
	parts := strings.Split(value, ".")
	if len(value) == 0 || len(value) > 1024 || (len(parts) != 5 && len(parts) != 6) || parts[0] != "amux1" {
		return invalid()
	}
	for _, value := range []string{parts[1], parts[3]} {
		id, parseErr := uuid.Parse(value)
		if parseErr != nil || id == uuid.Nil || id.String() != value {
			return invalid()
		}
	}
	for _, value := range []string{parts[2], parts[4]} {
		if len(value) != 64 {
			return invalid()
		}
		if _, parseErr := hex.DecodeString(value); parseErr != nil {
			return invalid()
		}
	}
	port := uint64(24181)
	if len(parts) == 6 {
		port, err = strconv.ParseUint(parts[5], 10, 16)
		if err != nil || port == 0 {
			return invalid()
		}
	}
	return parts[1], parts[2], parts[3], uint16(port), nil
}

func pairingBaseURL(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("server address must be an HTTPS origin without credentials, path, query or fragment")
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "24182")
	} else {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid secure server port")
		}
	}
	u.Path = ""
	u.RawPath = ""
	return u, nil
}

func validatePinnedDiscovery(discovery SecurityDiscovery, cluster, pin string, base *url.URL) (*x509.Certificate, []byte, error) {
	if discovery.Version != 1 || discovery.ClusterID != cluster || discovery.RootSHA256 != pin ||
		discovery.SecurePort == 0 || portOrDefault(base) != discovery.SecurePort {
		return nil, nil, fmt.Errorf("server discovery does not match the peering key")
	}
	block, rest := pem.Decode([]byte(discovery.RootPEM))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, fmt.Errorf("discovery must provide exactly one installation root")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid discovery root certificate")
	}
	fingerprint := sha256.Sum256(root.Raw)
	if hex.EncodeToString(fingerprint[:]) != pin || !root.IsCA || !root.BasicConstraintsValid ||
		root.KeyUsage&x509.KeyUsageCertSign == 0 || root.SubjectKeyId == nil ||
		!bytes.Equal(root.RawSubject, root.RawIssuer) || root.CheckSignatureFrom(root) != nil ||
		root.Subject.CommonName != "amux "+cluster || time.Now().Before(root.NotBefore) || time.Now().After(root.NotAfter) {
		return nil, nil, fmt.Errorf("discovery trust root does not match the peering key")
	}
	return root, append([]byte(nil), root.Raw...), nil
}

func portOrDefault(u *url.URL) uint16 {
	port := u.Port()
	if port == "" {
		return 24182
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(number)
}

func pairingEndpoint(base *url.URL, path string) string {
	endpoint := *base
	endpoint.Path = "/" + strings.TrimLeft(path, "/")
	endpoint.RawPath = ""
	return endpoint.String()
}

func getPairingResponse(ctx context.Context, client *http.Client, method, endpoint string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("invalid server endpoint")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return client.Do(request)
}

func decodePairingResponse(response *http.Response, value any) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusForbidden {
			var rejection struct {
				Code EnrollmentRejection `json:"error"`
			}
			decoder := json.NewDecoder(io.LimitReader(response.Body, peerHTTPBodyLimit))
			if decoder.Decode(&rejection) == nil {
				if message := rejection.Code.Message(); message != "" {
					return fmt.Errorf("server returned HTTP %d: %s", response.StatusCode, message)
				}
			}
		}
		return fmt.Errorf("server returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, peerHTTPBodyLimit+1))
	if err != nil || len(body) > peerHTTPBodyLimit {
		return fmt.Errorf("invalid server response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid server response")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid server response")
	}
	return nil
}

func pinnedEnrollmentClient(root *x509.Certificate, timeout time.Duration) (*http.Client, error) {
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return &http.Client{
		Transport:     &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}},
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func privatePairingDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func pairingInstallationID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "installation-id")
	exists, err := regularProfileFile(path)
	if err != nil {
		return "", err
	}
	if exists {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return canonicalPairingUUID(strings.TrimSpace(string(data)))
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	value := id.String()
	if err := writePrivateExclusive(path, []byte(value)); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		return canonicalPairingUUID(strings.TrimSpace(string(data)))
	}
	return value, nil
}

func canonicalPairingUUID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return "", fmt.Errorf("invalid installation identity")
	}
	return value, nil
}

func newRemotePairingPending(directory, invitationID string) (remotePairingPending, error) {
	var pending remotePairingPending
	id, err := uuid.NewRandom()
	if err != nil {
		return pending, err
	}
	keyPEM, csrPEM, err := createEnrollmentCSR()
	if err != nil {
		return pending, err
	}
	keyPath := filepath.Join(directory, "client-key-"+id.String()+".pem")
	protected, err := protectProfileRootKey(keyPEM)
	if err != nil {
		return pending, err
	}
	if err := replacePrivateFile(keyPath, protected); err != nil {
		return pending, err
	}
	return remotePairingPending{
		RequestID: id.String(), CSRPEM: string(csrPEM), KeyPath: keyPath,
		RootPath: filepath.Join(directory, "root.pem"), InvitationID: invitationID,
	}, nil
}

func loadRemotePairingPending(directory, path, invitationID, rootPath string) (remotePairingPending, error) {
	var pending remotePairingPending
	exists, err := regularProfileFile(path)
	if err != nil {
		return pending, err
	}
	if !exists {
		return pending, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pending, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		return pending, fmt.Errorf("invalid pending credential operation")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return pending, fmt.Errorf("invalid pending credential operation")
	}
	if _, err := canonicalPairingUUID(pending.RequestID); err != nil {
		return pending, fmt.Errorf("invalid pending credential operation")
	}
	if _, err := canonicalPairingUUID(pending.InvitationID); err != nil {
		return pending, fmt.Errorf("invalid pending credential operation")
	}
	if pending.RootPath != "" && pending.RootPath != rootPath {
		return pending, fmt.Errorf("pending root path escaped peer profile")
	}
	expectedKey := filepath.Join(directory, "client-key-"+pending.RequestID+".pem")
	if pending.KeyPath != expectedKey {
		return pending, fmt.Errorf("pending private key escaped peer profile")
	}
	keyExists, err := regularProfileFile(expectedKey)
	if err != nil || !keyExists {
		return pending, fmt.Errorf("pending client private key is unavailable")
	}
	block, rest := pem.Decode([]byte(pending.CSRPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		return pending, fmt.Errorf("invalid pending enrollment request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return pending, fmt.Errorf("invalid pending enrollment request")
	}
	if pending.RootPath == "" {
		pending.RootPath = rootPath
	}
	if invitationID == "" {
		return pending, fmt.Errorf("invalid peering key")
	}
	return pending, nil
}

func saveRemotePairingPending(path string, pending remotePairingPending) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return replacePrivateFile(path, data)
}

func validateIssuedPeerCredential(response EnrollmentResponse, pending remotePairingPending, cluster, pin string, root *x509.Certificate, rootDER []byte) error {
	if response.ClusterID != cluster || response.NodeID == "" || response.CertificatePEM == "" || response.Serial == "" ||
		response.ExpiresAt <= time.Now().UnixMilli() {
		return fmt.Errorf("issued certificate belongs to another server or is expired")
	}
	if _, err := canonicalPairingUUID(response.NodeID); err != nil {
		return fmt.Errorf("invalid issued node identity")
	}
	rootBlock, rest := pem.Decode([]byte(response.RootPEM))
	if rootBlock == nil || rootBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || !bytes.Equal(rootBlock.Bytes, rootDER) {
		return fmt.Errorf("issued certificate root differs from the peering key")
	}
	rootSum := sha256.Sum256(rootBlock.Bytes)
	if hex.EncodeToString(rootSum[:]) != pin {
		return fmt.Errorf("issued certificate root differs from the peering key")
	}
	block, rest := pem.Decode([]byte(response.CertificatePEM))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("invalid issued client certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("invalid issued client certificate")
	}
	serial, serialErr := certificateSerialCompat(certificate.Raw)
	if serialErr != nil || serial != response.Serial || certificate.Subject.CommonName != response.NodeID ||
		certificate.NotAfter.UnixMilli() != response.ExpiresAt || time.Now().Before(certificate.NotBefore) ||
		time.Now().After(certificate.NotAfter) || !validLocalClientCertificate(certificate, root) {
		return fmt.Errorf("issued certificate identity is unconfirmed")
	}
	keyPath := pending.KeyPath
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("pending client private key is unavailable")
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return fmt.Errorf("cannot read pending client private key")
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return fmt.Errorf("invalid pending client private key")
	}
	public, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || public.X.Cmp(key.X) != 0 || public.Y.Cmp(key.Y) != 0 {
		return fmt.Errorf("issued certificate does not match pending client key")
	}
	return nil
}

func activateRemotePairing(ctx context.Context, base *url.URL, directory, activePath, instanceID string, pending remotePairingPending) (LocalSecurityProfile, error) {
	response := pending.Response
	if response == nil {
		return LocalSecurityProfile{}, fmt.Errorf("certificate issuance is unconfirmed")
	}
	serialHash := sha256.Sum256([]byte(response.Serial))
	certificatePath := filepath.Join(directory, fmt.Sprintf("client-%x.pem", serialHash[:]))
	if err := replacePrivateFile(certificatePath, []byte(response.CertificatePEM)); err != nil {
		return LocalSecurityProfile{}, err
	}
	profile := LocalSecurityProfile{
		Version: 1, ClusterID: response.ClusterID, NodeID: response.NodeID, InstanceID: instanceID,
		URL: base.String(), RootPEMPath: pending.RootPath, CertificatePEMPath: certificatePath,
		PrivateKeyPEMPath: pending.KeyPath, Serial: response.Serial, ExpiresAt: response.ExpiresAt,
	}
	stagedPath := filepath.Join(directory, "client-profile.staged.json")
	if err := saveLocalProfile(stagedPath, profile); err != nil {
		return LocalSecurityProfile{}, err
	}
	keyPEM, err := os.ReadFile(pending.KeyPath)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	identity, err := tls.X509KeyPair([]byte(response.CertificatePEM), keyPEM)
	if err != nil {
		return LocalSecurityProfile{}, fmt.Errorf("issued certificate and private key do not match")
	}
	rootPEM, err := os.ReadFile(pending.RootPath)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	block, _ := pem.Decode(rootPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return LocalSecurityProfile{}, fmt.Errorf("invalid pinned server root")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return LocalSecurityProfile{}, fmt.Errorf("invalid pinned server root")
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	identity.Leaf, err = x509.ParseCertificate(identity.Certificate[0])
	if err != nil {
		return LocalSecurityProfile{}, fmt.Errorf("invalid issued client certificate")
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{identity},
		}},
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	activateResponse, err := getPairingResponse(ctx, client, http.MethodPost, pairingEndpoint(base, "v1/enrollment/activate"), nil)
	if err != nil {
		return LocalSecurityProfile{}, fmt.Errorf("certificate activation failed: %w", err)
	}
	var activated AuthorizedSecurityNode
	if err := decodePairingResponse(activateResponse, &activated); err != nil || activated.NodeID != response.NodeID ||
		activated.CurrentSerial != response.Serial || activated.Revoked {
		return LocalSecurityProfile{}, fmt.Errorf("certificate activation is unconfirmed")
	}
	if err := saveLocalProfile(activePath, profile); err != nil {
		return LocalSecurityProfile{}, fmt.Errorf("publish enrolled client profile: %w", err)
	}
	_ = os.Remove(stagedPath)
	return profile, nil
}

func createEnrollmentCSR() (keyPEM, csrPEM []byte, err error) {
	identity, err := generateLocalClientIdentity()
	if err != nil {
		return nil, nil, err
	}
	return identity.privateKeyPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: identity.csr.Raw}), nil
}
