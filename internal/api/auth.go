package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/curruwilla/processd/internal/config"
)

// hashPrefix is the only digest format accepted in the configuration.
const hashPrefix = "sha256:"

type contextKey int

const tokenContextKey contextKey = iota

// HashToken returns the configuration value for a plaintext token. The secret
// itself is never stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hashPrefix + hex.EncodeToString(sum[:])
}

// authenticator resolves bearer tokens against the configured allowlist.
type authenticator struct {
	tokens []config.Token
}

func newAuthenticator(tokens []config.Token) *authenticator {
	return &authenticator{tokens: tokens}
}

// authenticate returns the matching token, comparing digests in constant time
// so that a wrong token cannot be discovered by timing the response.
func (a *authenticator) authenticate(header string) (config.Token, bool) {
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return config.Token{}, false
	}

	presented := []byte(HashToken(strings.TrimSpace(raw)))

	for _, token := range a.tokens {
		if subtle.ConstantTimeCompare(presented, []byte(token.Hash)) == 1 {
			return token, true
		}
	}

	return config.Token{}, false
}

// tokenFrom returns the authenticated token of a request.
func tokenFrom(ctx context.Context) (config.Token, bool) {
	token, ok := ctx.Value(tokenContextKey).(config.Token)
	return token, ok
}

// withToken stores the authenticated token on the request context.
func withToken(r *http.Request, token config.Token) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), tokenContextKey, token))
}
