package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

const peerRenewBefore = 10 * 24 * time.Hour

type peerRenewalPending struct {
	RequestID string              `json:"request_id"`
	CSRPEM    string              `json:"csr_pem"`
	KeyPath   string              `json:"key_path"`
	RootPath  string              `json:"root_path"`
	InviteID  *string             `json:"invitation_id"`
	Response  *EnrollmentResponse `json:"response"`
}

type peerPreviousIdentity struct {
	Profile  LocalSecurityProfile `json:"profile"`
	RetireAt int64                `json:"retire_at"`
}

// RenewPeerClientIfDue renews an enrolled peer when its certificate is within
// ten days of expiry. The profile path must identify client-profile.json.
// If supplied, validate checks the reloaded credentials under the renewal lock
// before any local mutation or network request.
func RenewPeerClientIfDue(ctx context.Context, profilePath string, validate func(PeerClientCredentials) error) error {
	return renewPeerClient(ctx, profilePath, false, validate)
}

// RenewPeerClientNow forces the same durable renewal path used by automatic renewal.
func RenewPeerClientNow(ctx context.Context, profilePath string) error {
	return renewPeerClient(ctx, profilePath, true, nil)
}

func renewPeerClient(ctx context.Context, profilePath string, force bool, validate func(PeerClientCredentials) error) error {
	path, err := filepath.Abs(profilePath)
	if err != nil {
		return fmt.Errorf("peer profile path is invalid")
	}
	if filepath.Base(path) == "local-client.json" {
		return nil
	}
	if filepath.Base(path) != "client-profile.json" {
		return fmt.Errorf("peer profile must name a client-profile.json manifest")
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("peer credential directory is unavailable")
	}
	path = filepath.Join(directory, filepath.Base(path))
	lock, err := lockPairingFile(filepath.Join(directory, "renew.lock"))
	if err != nil {
		return fmt.Errorf("peer renewal lock unavailable: %w", err)
	}
	defer lock.Close()

	credentials, err := LoadPeerClientCredentials(path)
	if err != nil {
		return err
	}
	if validate != nil {
		if err := validate(credentials); err != nil {
			return err
		}
	}
	active := credentials.Profile
	active.RootPEMPath, err = confinedProfilePath(directory, active.RootPEMPath, true)
	if err != nil {
		return err
	}
	active.CertificatePEMPath, err = confinedProfilePath(directory, active.CertificatePEMPath, true)
	if err != nil {
		return err
	}
	active.PrivateKeyPEMPath, err = confinedProfilePath(directory, active.PrivateKeyPEMPath, true)
	if err != nil {
		return err
	}
	if err := retirePeerPrevious(directory, active, time.Now()); err != nil {
		return err
	}
	pendingPath := filepath.Join(directory, "renew-pending.json")
	pendingExists, err := regularProfileFile(pendingPath)
	if err != nil {
		return err
	}
	if !force && !pendingExists && time.Until(time.UnixMilli(active.ExpiresAt)) > peerRenewBefore {
		return nil
	}
	pending, err := loadPeerRenewalPending(directory, pendingPath)
	if err != nil {
		return err
	}
	if pending.RequestID == "" {
		pending, err = newPeerRenewalPending(directory)
		if err != nil {
			return err
		}
	}
	pending.RootPath = active.RootPEMPath
	if err := savePeerRenewalPending(pendingPath, pending); err != nil {
		return err
	}
	base, err := pairingBaseURL(active.URL)
	if err != nil {
		return err
	}
	root, err := loadPeerRoot(active.RootPEMPath)
	if err != nil {
		return err
	}
	if pending.Response == nil {
		client, err := pinnedEnrollmentClient(root, 15*time.Second)
		if err != nil {
			return err
		}
		receiptURL := pairingEndpoint(base, "v1/enrollment/requests/"+pending.RequestID)
		response, requestErr := getPairingResponse(ctx, client, http.MethodGet, receiptURL, nil)
		client.CloseIdleConnections()
		if requestErr != nil {
			return fmt.Errorf("renewal receipt unavailable: %w", requestErr)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			var issued EnrollmentResponse
			if err := decodePairingResponse(response, &issued); err != nil {
				return fmt.Errorf("renewal receipt failed: %w", err)
			}
			pending.Response = &issued
		} else {
			status := response.StatusCode
			response.Body.Close()
			if status != http.StatusNotFound {
				return fmt.Errorf("renewal receipt could not be confirmed (HTTP %d); pending state was retained", status)
			}
			request := RenewalRequest{RequestID: pending.RequestID, CSRPEM: pending.CSRPEM}
			body, err := json.Marshal(request)
			if err != nil {
				return err
			}
			client, err := peerMutualTLSClient(credentials, 15*time.Second)
			if err != nil {
				return err
			}
			response, err = getPairingResponse(ctx, client, http.MethodPost, pairingEndpoint(base, "v1/enrollment/renew"), bytes.NewReader(body))
			client.CloseIdleConnections()
			if err != nil {
				return fmt.Errorf("certificate renewal unavailable: %w", err)
			}
			var issued EnrollmentResponse
			if err := decodePairingResponse(response, &issued); err != nil {
				return fmt.Errorf("certificate renewal failed: %w", err)
			}
			pending.Response = &issued
		}
		pairPending := pending.asPairingPending()
		if err := validateIssuedPeerCredential(*pending.Response, pairPending, active.ClusterID, peerRootPin(root.Raw), root, root.Raw); err != nil {
			return err
		}
		if err := savePeerRenewalPending(pendingPath, pending); err != nil {
			return err
		}
	}
	if err := validateIssuedPeerCredential(*pending.Response, pending.asPairingPending(), active.ClusterID, peerRootPin(root.Raw), root, root.Raw); err != nil {
		return err
	}
	// If activation completed before an interruption, only clean up its receipt.
	if active.Serial == pending.Response.Serial && active.NodeID == pending.Response.NodeID {
		return os.Remove(pendingPath)
	}
	previousPath := filepath.Join(directory, "previous-profile.json")
	retireAt := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if active.ExpiresAt < retireAt {
		retireAt = active.ExpiresAt
	}
	previousData, err := json.Marshal(peerPreviousIdentity{Profile: active, RetireAt: retireAt})
	if err != nil {
		return err
	}
	if err := replacePrivateFile(previousPath, previousData); err != nil {
		return err
	}
	pairPending := pending.asPairingPending()
	if _, err := activateRemotePairing(ctx, base, directory, path, active.InstanceID, pairPending); err != nil {
		return err
	}
	_ = os.Remove(pendingPath)
	return nil
}

func newPeerRenewalPending(directory string) (peerRenewalPending, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return peerRenewalPending{}, err
	}
	keyPEM, csrPEM, err := createEnrollmentCSR()
	if err != nil {
		return peerRenewalPending{}, err
	}
	keyPEM, err = protectProfileRootKey(keyPEM)
	if err != nil {
		return peerRenewalPending{}, err
	}
	keyPath := filepath.Join(directory, "client-key-"+id.String()+".pem")
	if err := writePrivateExclusive(keyPath, keyPEM); err != nil {
		return peerRenewalPending{}, err
	}
	return peerRenewalPending{RequestID: id.String(), CSRPEM: string(csrPEM), KeyPath: keyPath}, nil
}

func loadPeerRenewalPending(directory, path string) (peerRenewalPending, error) {
	var pending peerRenewalPending
	exists, err := regularProfileFile(path)
	if err != nil || !exists {
		return pending, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pending, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		return pending, fmt.Errorf("invalid pending peer renewal")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return pending, fmt.Errorf("invalid pending peer renewal")
	}
	id, err := uuid.Parse(pending.RequestID)
	if err != nil || id == uuid.Nil || id.String() != pending.RequestID {
		return pending, fmt.Errorf("invalid pending peer renewal")
	}
	keyPath, err := confinedProfilePath(directory, pending.KeyPath, true)
	if err != nil || filepath.Base(keyPath) != "client-key-"+pending.RequestID+".pem" {
		return pending, fmt.Errorf("invalid pending peer renewal key")
	}
	pending.KeyPath = keyPath
	block, rest := pem.Decode([]byte(pending.CSRPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		return pending, fmt.Errorf("invalid pending peer renewal request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return pending, fmt.Errorf("invalid pending peer renewal request")
	}
	rootPath, err := confinedProfilePath(directory, pending.RootPath, true)
	if err == nil {
		pending.RootPath = rootPath
	}
	return pending, nil
}

func savePeerRenewalPending(path string, pending peerRenewalPending) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return replacePrivateFile(path, data)
}

func (pending peerRenewalPending) asPairingPending() remotePairingPending {
	return remotePairingPending{RequestID: pending.RequestID, CSRPEM: pending.CSRPEM, KeyPath: pending.KeyPath, RootPath: pending.RootPath, Response: pending.Response}
}

func loadPeerRoot(path string) (*x509.Certificate, error) {
	block, rest, err := readSingleCertificate(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("invalid peer trust root")
	}
	return x509.ParseCertificate(block.Bytes)
}

func readSingleCertificate(path string) (*pem.Block, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("invalid peer trust root")
	}
	return block, rest, nil
}

func peerRootPin(rootDER []byte) string {
	sum := sha256.Sum256(rootDER)
	return fmt.Sprintf("%x", sum[:])
}

func peerMutualTLSClient(credentials PeerClientCredentials, timeout time.Duration) (*http.Client, error) {
	identity := credentials.Certificate
	if identity.Leaf == nil && len(identity.Certificate) > 0 {
		leaf, err := x509.ParseCertificate(identity.Certificate[0])
		if err != nil {
			return nil, err
		}
		identity.Leaf = leaf
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: credentials.RootCAs, Certificates: []tls.Certificate{identity}}},
		Timeout:   timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func retirePeerPrevious(directory string, current LocalSecurityProfile, now time.Time) error {
	path := filepath.Join(directory, "previous-profile.json")
	exists, err := regularProfileFile(path)
	if err != nil || !exists {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var previous peerPreviousIdentity
	if err := json.Unmarshal(data, &previous); err != nil {
		return fmt.Errorf("invalid previous peer profile: %w", err)
	}
	if previous.RetireAt > now.UnixMilli() && previous.Profile.ExpiresAt > now.UnixMilli() {
		return nil
	}
	currentKey, err := confinedProfilePath(directory, current.PrivateKeyPEMPath, false)
	if err != nil {
		return err
	}
	currentCertificate, err := confinedProfilePath(directory, current.CertificatePEMPath, false)
	if err != nil {
		return err
	}
	for _, oldPath := range []string{previous.Profile.PrivateKeyPEMPath, previous.Profile.CertificatePEMPath} {
		confined, err := confinedProfilePath(directory, oldPath, false)
		if err != nil {
			return err
		}
		if confined == currentKey || confined == currentCertificate {
			continue
		}
		if err := os.Remove(confined); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
