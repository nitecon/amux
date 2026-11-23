package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
)

func pairedPeerForRenewal(t *testing.T, fixture *pairingFixture) storage.PeerProfile {
	t.Helper()
	address := fmt.Sprintf("https://127.0.0.1:%d", fixture.securePort)
	paired, err := storage.PairRemote(context.Background(), fixture.stateDir, address, fixture.invite.Invitation)
	if err != nil {
		t.Fatal(err)
	}
	return paired
}

func TestPeerRenewalDueAndForcedDurableActivation(t *testing.T) {
	fixture := newPairingFixture(t)
	paired := pairedPeerForRenewal(t, fixture)
	if err := storage.RenewPeerClientIfDue(context.Background(), paired.ProfilePath, nil); err != nil {
		t.Fatalf("not-due renewal: %v", err)
	}
	credentials, err := storage.LoadPeerClientCredentials(paired.ProfilePath)
	if err != nil || credentials.Profile.Serial != paired.Profile.Serial {
		t.Fatalf("not-due renewal changed active profile: %+v %v", credentials.Profile, err)
	}
	if err := storage.RenewPeerClientNow(context.Background(), paired.ProfilePath); err != nil {
		t.Fatal(err)
	}
	credentials, err = storage.LoadPeerClientCredentials(paired.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Profile.Serial == paired.Profile.Serial || credentials.Profile.NodeID != paired.Profile.NodeID {
		t.Fatalf("renewal profile identity mismatch: old=%+v new=%+v", paired.Profile, credentials.Profile)
	}
	if _, err := fixture.enrollment.AuthorizeCertificate(context.Background(), credentials.Certificate.Leaf); err != nil {
		t.Fatalf("renewed certificate is not active: %v", err)
	}
	previousPath := filepath.Join(filepath.Dir(paired.ProfilePath), "previous-profile.json")
	if _, err := os.Stat(previousPath); err != nil {
		t.Fatalf("previous profile recovery record missing: %v", err)
	}
	var previous struct {
		Profile  storage.LocalSecurityProfile `json:"profile"`
		RetireAt int64                        `json:"retire_at"`
	}
	previousData, err := os.ReadFile(previousPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(previousData, &previous); err != nil {
		t.Fatalf("decode previous profile record: %v", err)
	}
	previous.RetireAt = time.Now().Add(-time.Second).UnixMilli()
	previousData, err = json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previousPath, previousData, 0600); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("profile identity changed")
	if err := storage.RenewPeerClientIfDue(context.Background(), paired.ProfilePath, func(storage.PeerClientCredentials) error {
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("renewal skipped the locked credential check: %v", err)
	}
	if _, err := os.Stat(previousPath); err != nil {
		t.Fatalf("renewal changed the profile after identity rejection: %v", err)
	}
	if err := storage.RenewPeerClientIfDue(context.Background(), paired.ProfilePath, nil); err != nil {
		t.Fatalf("retire drained previous profile: %v", err)
	}
	for _, oldPath := range []string{previous.Profile.PrivateKeyPEMPath, previous.Profile.CertificatePEMPath} {
		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Fatalf("retired credential still exists at %s: %v", oldPath, err)
		}
	}
	if _, err := os.Stat(previousPath); !os.IsNotExist(err) {
		t.Fatalf("retired profile manifest still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(paired.ProfilePath), "renew-pending.json")); !os.IsNotExist(err) {
		t.Fatalf("renewal pending record remains after activation: %v", err)
	}
}

func TestPeerRenewalCanceledRequestResumesFromPendingState(t *testing.T) {
	fixture := newPairingFixture(t)
	paired := pairedPeerForRenewal(t, fixture)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.RenewPeerClientNow(canceled, paired.ProfilePath); err == nil {
		t.Fatal("canceled renewal unexpectedly completed")
	}
	pendingPath := filepath.Join(filepath.Dir(paired.ProfilePath), "renew-pending.json")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("renewal intent was not retained across interruption: %v", err)
	}
	if err := storage.RenewPeerClientNow(context.Background(), paired.ProfilePath); err != nil {
		t.Fatalf("resume renewal: %v", err)
	}
	credentials, err := storage.LoadPeerClientCredentials(paired.ProfilePath)
	if err != nil || credentials.Profile.Serial == paired.Profile.Serial {
		t.Fatalf("resumed renewal did not publish replacement: %+v %v", credentials.Profile, err)
	}
}

func TestPeerRenewalConcurrentCallsKeepOneActiveGeneration(t *testing.T) {
	fixture := newPairingFixture(t)
	paired := pairedPeerForRenewal(t, fixture)
	start := make(chan struct{})
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errors <- storage.RenewPeerClientNow(context.Background(), paired.ProfilePath)
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		// A concurrent caller can lose the nonblocking profile lock; it must not
		// create or publish a second generation.
		if err != nil {
			t.Logf("concurrent renewal did not acquire lock: %v", err)
		}
	}
	credentials, err := storage.LoadPeerClientCredentials(paired.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Profile.Serial == paired.Profile.Serial {
		t.Fatal("concurrent renewal did not publish a replacement")
	}
	if _, err := fixture.enrollment.AuthorizeCertificate(context.Background(), credentials.Certificate.Leaf); err != nil {
		t.Fatalf("published concurrent result is not active: %v", err)
	}
}
