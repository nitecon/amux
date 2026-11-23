package browserauth

import (
	"strings"
	"testing"
)

const rustArgon2DefaultFixture = "$argon2id$v=19$m=19456,t=2,p=1$MDEyMzQ1Njc4OWFiY2RlZg$69I7qCjDTsQwhQqFvpQJiqEI0lGQvxC84fXE0Ktxca4"

func TestVerifyPasswordAcceptsRustArgon2PHC(t *testing.T) {
	if !VerifyPassword("correct horse battery", rustArgon2DefaultFixture) {
		t.Fatal("Rust Argon2id default PHC did not verify")
	}
	if VerifyPassword("incorrect horse battery", rustArgon2DefaultFixture) {
		t.Fatal("wrong password verified")
	}
}

func TestHashPasswordUsesRustDefaultsAndUnicodeCharacterPolicy(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if _, err := HashPassword(string([]byte{0xff, 'a', 'b', 'c', 'd', 'e', 'f', 'g'})); err == nil {
		t.Fatal("invalid UTF-8 password accepted")
	}
	phc, err := HashPassword("éééééééé")
	if err != nil {
		t.Fatalf("eight Unicode characters rejected: %v", err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("generated PHC does not use Rust Argon2 defaults: %q", phc)
	}
	if !VerifyPassword("éééééééé", phc) || VerifyPassword("ééééééé", phc) {
		t.Fatal("generated PHC failed password or Unicode character-count checks")
	}
	second, err := HashPassword("éééééééé")
	if err != nil {
		t.Fatal(err)
	}
	if phc == second {
		t.Fatal("password hashes reused the random salt")
	}
}

func TestVerifyPasswordRejectsMalformedAndUnboundedPHC(t *testing.T) {
	for _, phc := range []string{
		"not-a-phc",
		strings.Replace(rustArgon2DefaultFixture, "$argon2id$", "$argon2i$", 1),
		strings.Replace(rustArgon2DefaultFixture, "v=19", "v=16", 1),
		strings.Replace(rustArgon2DefaultFixture, "m=19456", "m=65537", 1),
		strings.Replace(rustArgon2DefaultFixture, "t=2", "t=11", 1),
		strings.Replace(rustArgon2DefaultFixture, "p=1", "p=9", 1),
		strings.Replace(rustArgon2DefaultFixture, "m=19456,", "m=19456,m=19456,", 1),
		strings.Replace(rustArgon2DefaultFixture, "p=1", "p=1,x=2", 1),
		strings.Replace(rustArgon2DefaultFixture, "$MDEy", "$a", 1),
		strings.Repeat("x", maxPHCLength+1),
	} {
		if VerifyPassword("correct horse battery", phc) {
			t.Errorf("invalid or unbounded PHC verified: %q", phc)
		}
	}
}
