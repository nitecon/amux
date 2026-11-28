package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	MinimumPasswordLength = 8
	defaultMemoryKiB      = 19 * 1024
	defaultIterations     = 2
	defaultParallelism    = 1
	defaultHashLength     = 32
	saltLength            = 16
	maxPHCLength          = 1024
	maxMemoryKiB          = 64 * 1024
	maxIterations         = 4
	maxParallelism        = 4
	maxSaltLength         = 64
	maxHashLength         = 64
)

var phcBase64 = base64.RawStdEncoding.Strict()

// HashPassword returns a Rust-compatible Argon2id PHC string with a fresh
// cryptographic salt. Password length is counted in Unicode code points, as
// Rust's str::chars().count() does.
func HashPassword(password string) (string, error) {
	if !utf8.ValidString(password) {
		return "", fmt.Errorf("password must be valid UTF-8")
	}
	if utf8.RuneCountInString(password) < MinimumPasswordLength {
		return "", fmt.Errorf("password must be at least %d characters", MinimumPasswordLength)
	}
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, defaultIterations, defaultMemoryKiB, defaultParallelism, defaultHashLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		defaultMemoryKiB, defaultIterations, defaultParallelism,
		phcBase64.EncodeToString(salt), phcBase64.EncodeToString(hash)), nil
}

// VerifyPassword verifies an Argon2id PHC hash, rejecting malformed or
// resource-heavy parameters before deriving the candidate digest.
func VerifyPassword(password, phc string) bool {
	if len(phc) == 0 || len(phc) > maxPHCLength || !utf8.ValidString(password) {
		return false
	}
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	memory, iterations, parallelism, ok := parsePHCParams(parts[3])
	if !ok || memory < 8*parallelism || memory > maxMemoryKiB || iterations == 0 || iterations > maxIterations || parallelism == 0 || parallelism > maxParallelism {
		return false
	}
	salt, err := phcBase64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > maxSaltLength {
		return false
	}
	expected, err := phcBase64.DecodeString(parts[5])
	if err != nil || len(expected) < 4 || len(expected) > maxHashLength {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, uint8(parallelism), uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func parsePHCParams(value string) (memory, iterations, parallelism uint32, ok bool) {
	var seenMemory, seenIterations, seenParallelism bool
	for _, item := range strings.Split(value, ",") {
		key, raw, found := strings.Cut(item, "=")
		if !found || raw == "" {
			return 0, 0, 0, false
		}
		for i := 0; i < len(raw); i++ {
			if raw[i] < '0' || raw[i] > '9' {
				return 0, 0, 0, false
			}
		}
		parsed, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		switch key {
		case "m":
			if seenMemory {
				return 0, 0, 0, false
			}
			memory, seenMemory = uint32(parsed), true
		case "t":
			if seenIterations {
				return 0, 0, 0, false
			}
			iterations, seenIterations = uint32(parsed), true
		case "p":
			if seenParallelism {
				return 0, 0, 0, false
			}
			parallelism, seenParallelism = uint32(parsed), true
		default:
			return 0, 0, 0, false
		}
	}
	return memory, iterations, parallelism, seenMemory && seenIterations && seenParallelism
}
