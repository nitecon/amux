package storage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureProfileServerTLSCreatesAndReusesSANLeaf(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecurityRoot(profile, clusterID); err != nil {
		t.Fatal(err)
	}
	names := []string{"localhost", "127.0.0.1"}
	first, err := EnsureProfileServerTLS(profile, clusterID, names)
	if err != nil {
		t.Fatal(err)
	}
	root := parseTestProfileRoot(t, filepath.Join(profile, "root.pem"))
	leaf := first.Leaf
	if leaf == nil || leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatal("leaf certificate has incorrect CA or TLS usage")
	}
	if !sameCertificateNameSet(certificateNames(leaf), names) {
		t.Fatalf("leaf SANs=%v, want %v", certificateNames(leaf), names)
	}
	if !bytes.Equal(leaf.AuthorityKeyId, root.SubjectKeyId) || len(leaf.AuthorityKeyId) == 0 {
		t.Fatal("leaf authority key identifier does not match profile root")
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("verify localhost chain: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "127.0.0.1", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("verify IP SAN chain: %v", err)
	}
	manifestBefore := readFile(t, filepath.Join(profile, "server-identity.json"))
	second, err := EnsureProfileServerTLS(profile, clusterID, names)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) || !bytes.Equal(manifestBefore, readFile(t, filepath.Join(profile, "server-identity.json"))) {
		t.Fatal("startup replaced a still-valid identity")
	}
	if _, ok := first.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatal("TLS private key is not ECDSA")
	}
}

func TestEnsureProfileServerTLSRotatesOnNameChangeAndKeepsPrevious(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := ensureProfileServerTLSAt(profile, clusterID, []string{"localhost"}, now)
	if err != nil {
		t.Fatal(err)
	}
	var original profileServerIdentity
	if err := json.Unmarshal(readFile(t, filepath.Join(profile, "server-identity.json")), &original); err != nil {
		t.Fatal(err)
	}
	second, err := ensureProfileServerTLSAt(profile, clusterID, []string{"localhost", "127.0.0.1"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("changed SAN set reused old leaf")
	}
	var manifest profileServerIdentity
	if err := json.Unmarshal(readFile(t, filepath.Join(profile, "server-identity.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Previous == nil || manifest.Previous.Serial != original.Serial {
		t.Fatalf("rotation manifest previous=%+v", manifest.Previous)
	}
	if _, err := os.Stat(manifest.Previous.Certificate); err != nil {
		t.Fatalf("prior certificate was not retained: %v", err)
	}
	if _, err := os.Stat(manifest.Previous.PrivateKey); err != nil {
		t.Fatalf("prior private key was not retained: %v", err)
	}
}

func TestEnsureProfileServerTLSFailsClosedOnInvalidManifest(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureProfileServerTLS(profile, clusterID, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(profile, "server-identity.json")
	validManifest := readFile(t, manifestPath)
	tests := []struct {
		name string
		data []byte
	}{
		{name: "invalid JSON", data: []byte("{")},
		{name: "outside profile key path", data: func() []byte {
			var identity profileServerIdentity
			if err := json.Unmarshal(validManifest, &identity); err != nil {
				t.Fatal(err)
			}
			identity.PrivateKey = filepath.Join(t.TempDir(), "outside-key.pem")
			data, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(manifestPath, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureProfileServerTLS(profile, clusterID, []string{"localhost"}); err == nil {
				t.Fatal("invalid manifest accepted")
			}
			if !bytes.Equal(test.data, readFile(t, manifestPath)) {
				t.Fatal("failed validation replaced manifest")
			}
		})
	}
}

func TestEnsureProfileServerTLSReadsRustSerialWithSignPadding(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecurityRoot(profile, clusterID); err != nil {
		t.Fatal(err)
	}
	root, rootKey, err := loadProfileRoot(profile)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expires := now.Add(profileServerLeafLifetime).Truncate(time.Second)
	template := &x509.Certificate{
		SerialNumber:       big.NewInt(128),
		Subject:            pkix.Name{CommonName: "rcgen self signed cert"},
		NotBefore:          now.Add(-5 * time.Minute).Truncate(time.Second),
		NotAfter:           expires,
		KeyUsage:           x509.KeyUsageDigitalSignature,
		ExtKeyUsage:        []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:           []string{"localhost"},
		AuthorityKeyId:     append([]byte(nil), root.SubjectKeyId...),
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := certificateSerialCompat(der)
	if err != nil || serial != "00:80" {
		t.Fatalf("source-compatible serial=%q err=%v, want leading sign pad 00:80", serial, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := protectProfileRootKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	canonicalProfile, err := filepath.EvalSymlinks(profile)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(canonicalProfile, "server-legacy.pem")
	keyPath := filepath.Join(canonicalProfile, "server-legacy-key.pem")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	legacy := profileServerIdentity{
		Serial:      serial,
		Certificate: certificatePath,
		PrivateKey:  keyPath,
		ExpiresAt:   expires.UnixMilli(),
		Names:       []string{"localhost"},
	}
	manifest, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(profile, "server-identity.json")
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := EnsureProfileServerTLS(profile, clusterID, []string{"localhost"})
	if err != nil {
		t.Fatal("read Rust-compatible serial manifest", err)
	}
	if !bytes.Equal(loaded.Certificate[0], der) || !bytes.Equal(readFile(t, manifestPath), manifest) {
		t.Fatal("legacy serial manifest was unexpectedly rotated or rewritten")
	}
}

func parseTestProfileRoot(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(readFile(t, path))
	if block == nil {
		t.Fatal("root PEM missing")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestEnsureProfileServerTLSLiveRequiresObservationAndDrain(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	first, err := ensureProfileServerTLSAt(profile, clusterID, []string{"localhost"}, now)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity := readProfileServerIdentity(t, profile)
	second, err := EnsureProfileServerTLSLive(profile, clusterID, []string{"localhost", "127.0.0.1"}, ProfileServerLeaseSnapshot{}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("name change did not rotate server leaf")
	}
	secondIdentity := readProfileServerIdentity(t, profile)
	if secondIdentity.Previous == nil || secondIdentity.Previous.Serial != firstIdentity.Serial {
		t.Fatalf("previous generation missing: %+v", secondIdentity.Previous)
	}
	oldCertificate, oldKey := secondIdentity.Previous.Certificate, secondIdentity.Previous.PrivateKey

	// Zero old leases is insufficient until a successful handshake has observed the new leaf.
	returned, err := EnsureProfileServerTLSLive(profile, clusterID, secondIdentity.Names, ProfileServerLeaseSnapshot{ActiveSerialCounts: map[string]int{firstIdentity.Serial: 0}, ObservedSerials: map[string]bool{firstIdentity.Serial: true}}, now.Add(2*time.Minute))
	if err != nil || !bytes.Equal(returned.Certificate[0], second.Certificate[0]) {
		t.Fatalf("current generation changed before observation: %v", err)
	}
	if got := readProfileServerIdentity(t, profile).Previous; got == nil || got.Serial != firstIdentity.Serial {
		t.Fatalf("unobserved generation retired: %+v", got)
	}

	// Once the new serial is observed, an active old socket still keeps its files.
	leases := ProfileServerLeaseSnapshot{ActiveSerialCounts: map[string]int{firstIdentity.Serial: 1}, ObservedSerials: map[string]bool{secondIdentity.Serial: true}}
	returned, err = EnsureProfileServerTLSLive(profile, clusterID, secondIdentity.Names, leases, now.Add(3*time.Minute))
	if err != nil || !bytes.Equal(returned.Certificate[0], second.Certificate[0]) {
		t.Fatalf("active previous generation disrupted: %v", err)
	}
	if _, err = os.Stat(oldCertificate); err != nil {
		t.Fatalf("leased previous cert removed: %v", err)
	}
	if _, err = os.Stat(oldKey); err != nil {
		t.Fatalf("leased previous key removed: %v", err)
	}

	// Observation plus zero leases permits retirement without changing current leaf.
	leases.ActiveSerialCounts[firstIdentity.Serial] = 0
	returned, err = EnsureProfileServerTLSLive(profile, clusterID, secondIdentity.Names, leases, now.Add(4*time.Minute))
	if err != nil || !bytes.Equal(returned.Certificate[0], second.Certificate[0]) {
		t.Fatalf("retirement changed current generation: %v", err)
	}
	if identity := readProfileServerIdentity(t, profile); identity.Previous != nil {
		t.Fatalf("drained previous generation remains: %+v", identity.Previous)
	}
	for _, path := range []string{oldCertificate, oldKey} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("retired file remains %s: %v", path, err)
		}
	}
}

func TestEnsureProfileServerTLSLiveBlocksSecondRotationUntilRetirement(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, err = ensureProfileServerTLSAt(profile, clusterID, []string{"localhost"}, now)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity := readProfileServerIdentity(t, profile)
	second, err := EnsureProfileServerTLSLive(profile, clusterID, []string{"localhost", "127.0.0.1"}, ProfileServerLeaseSnapshot{}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, filepath.Join(profile, "server-identity.json"))
	identity := readProfileServerIdentity(t, profile)
	previous := identity.Previous
	if previous == nil {
		t.Fatal("first rotation did not retain previous generation")
	}

	thirdNames := []string{"localhost", "127.0.0.1", "example.com"}
	blocked, err := EnsureProfileServerTLSLive(profile, clusterID, thirdNames, ProfileServerLeaseSnapshot{ObservedSerials: map[string]bool{firstIdentity.Serial: true}}, now.Add(2*time.Minute))
	if err == nil || !errors.Is(err, ErrProfileServerRotationBlocked) {
		t.Fatalf("rotation without observing current generation should block: %v", err)
	}
	if !bytes.Equal(blocked.Certificate[0], second.Certificate[0]) || !bytes.Equal(before, readFile(t, filepath.Join(profile, "server-identity.json"))) {
		t.Fatal("blocked rotation replaced current state")
	}

	leases := ProfileServerLeaseSnapshot{ActiveSerialCounts: map[string]int{previous.Serial: 3}, ObservedSerials: map[string]bool{identity.Serial: true}}
	blocked, err = EnsureProfileServerTLSLive(profile, clusterID, thirdNames, leases, now.Add(3*time.Minute))
	if err == nil || !errors.Is(err, ErrProfileServerRotationBlocked) {
		t.Fatalf("rotation with active previous lease should block: %v", err)
	}
	if !bytes.Equal(blocked.Certificate[0], second.Certificate[0]) {
		t.Fatal("blocked rotation did not return current leaf")
	}

	leases.ActiveSerialCounts[previous.Serial] = 0
	rotated, err := EnsureProfileServerTLSLive(profile, clusterID, thirdNames, leases, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rotated.Certificate[0], second.Certificate[0]) {
		t.Fatal("rotation did not proceed after previous retirement")
	}
	final := readProfileServerIdentity(t, profile)
	if final.Previous == nil || final.Previous.Serial != identity.Serial {
		t.Fatalf("new previous generation mismatch: %+v", final.Previous)
	}
	if _, err = os.Stat(previous.Certificate); !os.IsNotExist(err) {
		t.Fatalf("retired oldest cert remains: %v", err)
	}
}

func TestEnsureProfileServerTLSLiveDeadlineOverridesLeases(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, err = ensureProfileServerTLSAt(profile, clusterID, []string{"localhost"}, now)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity := readProfileServerIdentity(t, profile)
	second, err := EnsureProfileServerTLSLive(profile, clusterID, []string{"localhost", "127.0.0.1"}, ProfileServerLeaseSnapshot{}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	identity := readProfileServerIdentity(t, profile)
	previous := identity.Previous
	if previous == nil {
		t.Fatal("rotation did not retain previous")
	}
	returned, err := EnsureProfileServerTLSLive(profile, clusterID, identity.Names, ProfileServerLeaseSnapshot{ActiveSerialCounts: map[string]int{firstIdentity.Serial: 9}}, time.UnixMilli(previous.Until))
	if err != nil || !bytes.Equal(returned.Certificate[0], second.Certificate[0]) {
		t.Fatalf("deadline retirement replaced current leaf: %v", err)
	}
	if final := readProfileServerIdentity(t, profile); final.Previous != nil {
		t.Fatalf("deadline did not retire previous generation: %+v", final.Previous)
	}
	for _, path := range []string{previous.Certificate, previous.PrivateKey} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("deadline-retired file remains: %s %v", path, err)
		}
	}
}

func readProfileServerIdentity(t *testing.T, profile string) profileServerIdentity {
	t.Helper()
	var identity profileServerIdentity
	if err := json.Unmarshal(readFile(t, filepath.Join(profile, "server-identity.json")), &identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestConfinedProfilePathAcceptsCanonicalRustManifestThroughAlias(t *testing.T) {
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "profile")
	if err := os.Symlink(profile, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	credential := filepath.Join(profile, "local-key.pem")
	if err := os.WriteFile(credential, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{
		{alias, credential},
		{profile, filepath.Join(alias, "local-key.pem")},
	} {
		if got, err := confinedProfilePath(paths[0], paths[1], true); err != nil || got != credential {
			t.Fatalf("credential resolution = %q, %v", got, err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.pem")
	if err := os.WriteFile(outside, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := confinedProfilePath(alias, outside, true); err == nil {
		t.Fatal("accepted a credential outside the profile")
	}
	link := filepath.Join(profile, "linked-key.pem")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := confinedProfilePath(alias, link, true); err == nil {
		t.Fatal("accepted a symlinked credential")
	}
}
