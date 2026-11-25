package clientcontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// BuildHeader identifies the client executable used to start the process.
const BuildHeader = "X-Amux-Client-Build"

func ExecutableDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
