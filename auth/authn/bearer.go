package authn

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

func BearerValid(header, secret string) bool {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	want := sha256.Sum256([]byte(secret))
	supplied := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(supplied[:], want[:]) == 1
}
