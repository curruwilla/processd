package setup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the entropy of a generated token. 256 bits matches the digest
// the daemon stores, and leaves brute force out of the threat model.
const tokenBytes = 32

// GenerateToken mints an API token. The value is the secret itself: only its
// sha256 digest ever reaches the configuration.
func GenerateToken() (string, error) {
	buf := make([]byte, tokenBytes)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}

	return hex.EncodeToString(buf), nil
}
