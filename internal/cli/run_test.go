package cli

import (
	"errors"
	"maps"
	"testing"
)

func TestParseParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   []string
		want    map[string]string
		wantErr bool
	}{
		{name: "no params", input: nil, want: map[string]string{}},
		{name: "one pair", input: []string{"id=123"}, want: map[string]string{"id": "123"}},
		{
			name:  "several pairs",
			input: []string{"id=123", "mode=fast"},
			want:  map[string]string{"id": "123", "mode": "fast"},
		},
		{name: "value may contain =", input: []string{"query=a=b"}, want: map[string]string{"query": "a=b"}},
		{name: "empty value is allowed", input: []string{"id="}, want: map[string]string{"id": ""}},
		{name: "missing separator", input: []string{"id"}, wantErr: true},
		{name: "missing name", input: []string{"=123"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseParams(tt.input)

			if tt.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("parseParams(%q) returned %v, want a usage error", tt.input, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseParams(%q) returned %v, want nil", tt.input, err)
			}

			if !maps.Equal(got, tt.want) {
				t.Errorf("parseParams(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
