package api

import (
	"strings"
	"testing"

	"github.com/curruwilla/processd/internal/config"
)

func TestHashToken(t *testing.T) {
	t.Parallel()

	hash := HashToken("s3cret")

	if !strings.HasPrefix(hash, hashPrefix) {
		t.Errorf("HashToken() = %q, want it to start with %q", hash, hashPrefix)
	}

	if hash == HashToken("other") {
		t.Error("different tokens produced the same digest")
	}

	if hash != HashToken("s3cret") {
		t.Error("HashToken() is not deterministic")
	}
}

func TestAuthenticator_authenticate(t *testing.T) {
	t.Parallel()

	tokens := []config.Token{
		{Name: "billing", Hash: HashToken("billing-secret"), Workers: []string{"invoice"}},
		{Name: "ops", Hash: HashToken("ops-secret")},
	}

	auth := newAuthenticator(tokens)

	tests := []struct {
		name      string
		header    string
		wantOK    bool
		wantToken string
	}{
		{name: "valid token", header: "Bearer billing-secret", wantOK: true, wantToken: "billing"},
		{name: "second valid token", header: "Bearer ops-secret", wantOK: true, wantToken: "ops"},
		{name: "unknown token", header: "Bearer nope", wantOK: false},
		{name: "missing bearer prefix", header: "billing-secret", wantOK: false},
		{name: "empty header", header: "", wantOK: false},
		{name: "wrong scheme", header: "Basic billing-secret", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			token, ok := auth.authenticate(tt.header)
			if ok != tt.wantOK {
				t.Fatalf("authenticate(%q) ok = %t, want %t", tt.header, ok, tt.wantOK)
			}

			if ok && token.Name != tt.wantToken {
				t.Errorf("token = %q, want %q", token.Name, tt.wantToken)
			}
		})
	}
}

func TestToken_AllowsWorker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		token  config.Token
		worker string
		want   bool
	}{
		{name: "empty scope allows everything", token: config.Token{}, worker: "invoice", want: true},
		{
			name:   "listed worker is allowed",
			token:  config.Token{Workers: []string{"invoice", "report"}},
			worker: "report",
			want:   true,
		},
		{
			name:   "unlisted worker is denied",
			token:  config.Token{Workers: []string{"invoice"}},
			worker: "payroll",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.token.AllowsWorker(tt.worker); got != tt.want {
				t.Errorf("AllowsWorker(%q) = %t, want %t", tt.worker, got, tt.want)
			}
		})
	}
}
