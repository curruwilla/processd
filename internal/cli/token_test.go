package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/curruwilla/processd/internal/api"
)

func TestTokenHashCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stdin   string
		wantErr bool
	}{
		{name: "token on stdin", stdin: "s3cret\n"},
		{name: "trailing whitespace is trimmed", stdin: "  s3cret  \n"},
		{name: "no newline", stdin: "s3cret"},
		{name: "empty input", stdin: "\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := newTokenCommand()
			out := &bytes.Buffer{}

			cmd.SetOut(out)
			cmd.SetErr(out)
			cmd.SetIn(strings.NewReader(tt.stdin))
			cmd.SetArgs([]string{"hash"})

			err := cmd.Execute()

			if tt.wantErr {
				if err == nil {
					t.Fatal("Execute() returned nil, want an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("Execute() returned %v, want nil", err)
			}

			// The digest is what goes into auth.tokens[].hash, so it must match
			// exactly what the daemon computes.
			if got := strings.TrimSpace(out.String()); got != api.HashToken("s3cret") {
				t.Errorf("hash = %q, want %q", got, api.HashToken("s3cret"))
			}
		})
	}
}
