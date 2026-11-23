package storage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestEnsureSecurityRootRejectsLinkedCredentialFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows privileges")
	}
	profile := t.TempDir()
	rootPath := filepath.Join(profile, "root.pem")
	target := filepath.Join(t.TempDir(), "existing.pem")
	if err := os.WriteFile(target, []byte("existing fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, rootPath); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecurityRoot(profile, "fixture-cluster"); err == nil {
		t.Fatal("linked certificate was accepted")
	}
	if got := readFile(t, target); string(got) != "existing fixture" {
		t.Fatal("linked target changed")
	}
	if _, err := os.Lstat(filepath.Join(profile, "root-key.pem")); !os.IsNotExist(err) {
		t.Fatalf("startup created a root key beside a linked certificate: %v", err)
	}
}

func TestEnsureSecurityRootCreatesAndRetainsCA(t *testing.T) {
	profile := t.TempDir()
	clusterID := "profile-cluster"
	if err := EnsureSecurityRoot(profile, clusterID); err != nil {
		t.Fatal("create root", err)
	}
	rootPath := filepath.Join(profile, "root.pem")
	keyPath := filepath.Join(profile, "root-key.pem")
	certificatePEM := readFile(t, rootPath)
	keyPEM := readFile(t, keyPath)
	plainKey, err := unprotectProfileRootKey(keyPEM)
	if err != nil {
		t.Fatal("unprotect generated key", err)
	}
	assertRootPair(t, certificatePEM, plainKey, clusterID)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("root key permissions=%v", info.Mode().Perm())
		}
	}
	if err := EnsureSecurityRoot(profile, clusterID); err != nil {
		t.Fatal("reuse root", err)
	}
	if !bytes.Equal(certificatePEM, readFile(t, rootPath)) || !bytes.Equal(keyPEM, readFile(t, keyPath)) {
		t.Fatal("restart replaced valid root material")
	}
}

func TestEnsureSecurityRootPreservesSyntheticRustPairAndTightensKeyMode(t *testing.T) {
	profile := t.TempDir()
	clusterID := "fixture-cluster"
	certificatePEM, keyPEM := syntheticRootPair(t, clusterID)
	keyPEM, err := protectProfileRootKey(keyPEM)
	if err != nil {
		t.Fatal("protect synthetic root key", err)
	}
	certificatePath := filepath.Join(profile, "root.pem")
	keyPath := filepath.Join(profile, "root-key.pem")
	if err := os.WriteFile(certificatePath, certificatePEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecurityRoot(profile, clusterID); err != nil {
		t.Fatal("reuse synthetic Rust root", err)
	}
	if !bytes.Equal(certificatePEM, readFile(t, certificatePath)) || !bytes.Equal(keyPEM, readFile(t, keyPath)) {
		t.Fatal("valid existing root material changed")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("root key mode=%v, want 0600", info.Mode().Perm())
		}
	}
}

func TestEnsureSecurityRootFailsClosedForPartialOrMismatchedFiles(t *testing.T) {
	clusterID := "fixture-cluster"
	certA, keyA := syntheticRootPair(t, clusterID)
	_, keyB := syntheticRootPair(t, clusterID)
	var err error
	keyA, err = protectProfileRootKey(keyA)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err = protectProfileRootKey(keyB)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		cert    []byte
		key     []byte
		cluster string
	}{
		{name: "missing key", cert: certA},
		{name: "missing certificate", key: keyA},
		{name: "mismatched pair", cert: certA, key: keyB},
		{name: "wrong cluster binding", cert: certA, key: keyA, cluster: "another-cluster"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := t.TempDir()
			certPath, keyPath := filepath.Join(profile, "root.pem"), filepath.Join(profile, "root-key.pem")
			if test.cert != nil {
				if err := os.WriteFile(certPath, test.cert, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if test.key != nil {
				if err := os.WriteFile(keyPath, test.key, 0600); err != nil {
					t.Fatal(err)
				}
			}
			wantCluster := clusterID
			if test.cluster != "" {
				wantCluster = test.cluster
			}
			if err := EnsureSecurityRoot(profile, wantCluster); err == nil {
				t.Fatal("invalid root material accepted")
			}
			if test.cert != nil && !bytes.Equal(test.cert, readFile(t, certPath)) {
				t.Fatal("failed validation modified certificate")
			}
			if test.key != nil && !bytes.Equal(test.key, readFile(t, keyPath)) {
				t.Fatal("failed validation modified key")
			}
			if test.cert == nil {
				if _, err := os.Stat(certPath); !os.IsNotExist(err) {
					t.Fatal("failed validation created certificate")
				}
			}
			if test.key == nil {
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatal("failed validation created key")
				}
			}
		})
	}
}

func TestServerIdentityPublicationFailureKeepsPreviousGeneration(t *testing.T) {
	profile := t.TempDir()
	previousCertificate := filepath.Join(profile, "previous.pem")
	previousKey := filepath.Join(profile, "previous-key.pem")
	if err := os.WriteFile(previousCertificate, []byte("synthetic certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previousKey, []byte("synthetic key"), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(profile, "manifest-is-a-directory")
	if err := os.Mkdir(manifestPath, 0700); err != nil {
		t.Fatal(err)
	}
	previous := &profilePreviousServerIdentity{Certificate: previousCertificate, PrivateKey: previousKey}
	if err := publishProfileServerIdentity(manifestPath, profile, []byte(`{}`), previous); err == nil {
		t.Fatal("publication unexpectedly succeeded over a directory")
	}
	for _, path := range []string{previousCertificate, previousKey} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("prior generation was removed before publication: %v", err)
		}
	}
}

func syntheticRootPair(t *testing.T, clusterID string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyID := sha256.Sum256(publicKeyDER)
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "amux " + clusterID},
		NotBefore:             time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          append([]byte(nil), keyID[:20]...),
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func assertRootPair(t *testing.T, certificatePEM, keyPEM []byte, clusterID string) {
	t.Helper()
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil {
		t.Fatal("generated root certificate PEM is absent")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil || !validProfileRoot(certificate, mustParseRootKey(t, keyPEM), clusterID) {
		t.Fatal("generated root certificate/key pair is invalid")
	}
}

func mustParseRootKey(t *testing.T, keyPEM []byte) *ecdsa.PrivateKey {
	t.Helper()
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		t.Fatal("parse generated private key")
	}
	return key
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
