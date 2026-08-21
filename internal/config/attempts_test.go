package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAttempts_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		want    Attempts
		wantErr bool
	}{
		{name: "a bare count", yaml: "max_attempts: 5", want: 5},
		{name: "the unlimited keyword", yaml: "max_attempts: unlimited", want: AttemptsUnlimited},
		{name: "a quoted keyword", yaml: `max_attempts: "unlimited"`, want: AttemptsUnlimited},
		{name: "an omitted key", yaml: "{}", want: 0},
		{name: "anything else", yaml: "max_attempts: forever", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var decoded struct {
				MaxAttempts Attempts `yaml:"max_attempts"`
			}

			err := yaml.Unmarshal([]byte(tt.yaml), &decoded)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%q) returned nil, want an error", tt.yaml)
				}

				return
			}

			if err != nil {
				t.Fatalf("Unmarshal(%q) returned %v, want nil", tt.yaml, err)
			}

			if decoded.MaxAttempts != tt.want {
				t.Errorf("max_attempts = %v, want %v", decoded.MaxAttempts, tt.want)
			}
		})
	}
}

func TestAttempts_String(t *testing.T) {
	t.Parallel()

	if got := Attempts(AttemptsUnlimited).String(); got != "unlimited" {
		t.Errorf("String() = %q, want %q", got, "unlimited")
	}

	if got := Attempts(3).String(); got != "3" {
		t.Errorf("String() = %q, want %q", got, "3")
	}
}
