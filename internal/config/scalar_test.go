package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDuration_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{name: "seconds", input: "30s", want: 30 * time.Second},
		{name: "minutes", input: "5m", want: 5 * time.Minute},
		{name: "compound", input: "1h30m", want: 90 * time.Minute},
		{name: "days", input: "30d", want: 30 * 24 * time.Hour},
		{name: "weeks", input: "2w", want: 14 * 24 * time.Hour},
		{name: "fractional days", input: "0.5d", want: 12 * time.Hour},
		{name: "days cannot be combined", input: "1d12h", wantErr: true},
		{name: "not a duration", input: "soon", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var holder struct {
				Value Duration `yaml:"value"`
			}

			err := yaml.Unmarshal([]byte("value: "+tt.input), &holder)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%q) returned nil, want an error", tt.input)
				}

				return
			}

			if err != nil {
				t.Fatalf("Unmarshal(%q) returned %v, want nil", tt.input, err)
			}

			if holder.Value.Duration() != tt.want {
				t.Errorf("value = %s, want %s", holder.Value, tt.want)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "mebibytes", input: "32MiB", want: 32 << 20},
		{name: "gibibytes", input: "1GiB", want: 1 << 30},
		{name: "decimal megabytes", input: "32MB", want: 32_000_000},
		{name: "bytes suffix", input: "512B", want: 512},
		{name: "plain number", input: "1024", want: 1024},
		{name: "not a size", input: "big", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseByteSize(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseByteSize(%q) returned nil, want an error", tt.input)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseByteSize(%q) returned %v, want nil", tt.input, err)
			}

			if got.Bytes() != tt.want {
				t.Errorf("ParseByteSize(%q) = %d, want %d", tt.input, got.Bytes(), tt.want)
			}
		})
	}
}

func TestByteSize_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input ByteSize
		want  string
	}{
		{name: "exact mebibytes", input: 32 << 20, want: "32MiB"},
		{name: "exact kibibytes", input: 1024, want: "1KiB"},
		{name: "odd byte count", input: 3, want: "3B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.input.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
