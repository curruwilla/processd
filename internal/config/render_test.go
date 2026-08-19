package config

import (
	"errors"
	"slices"
	"testing"

	"github.com/curruwilla/processd/internal/core"
)

func TestWorker_Resolve(t *testing.T) {
	t.Parallel()

	worker := func() *Worker {
		return &Worker{
			Name:    "invoice",
			Command: "/usr/bin/php",
			Args: []string{
				"/var/www/app/artisan",
				"invoice:process",
				"--id={{id}}",
				"{{extra}}",
				"--verbose={{verbose}}",
			},
			Params: map[string]Param{
				"id":      {Required: true, Pattern: "^[0-9]{1,12}$"},
				"extra":   {},
				"verbose": {Enum: []string{"0", "1"}, Default: "0"},
			},
			Lock: "invoice:{{id}}",
		}
	}

	tests := []struct {
		name     string
		params   map[string]string
		wantArgs []string
		wantLock string
	}{
		{
			name:     "required param and default applied",
			params:   map[string]string{"id": "123"},
			wantArgs: []string{"/var/www/app/artisan", "invoice:process", "--id=123", "--verbose=0"},
			wantLock: "invoice:123",
		},
		{
			name:     "absent optional element is dropped, embedded one renders empty",
			params:   map[string]string{"id": "7", "verbose": "1"},
			wantArgs: []string{"/var/www/app/artisan", "invoice:process", "--id=7", "--verbose=1"},
			wantLock: "invoice:7",
		},
		{
			name:     "value with spaces stays a single argv element",
			params:   map[string]string{"id": "9", "extra": "--flag with space"},
			wantArgs: []string{"/var/www/app/artisan", "invoice:process", "--id=9", "--flag with space", "--verbose=0"},
			wantLock: "invoice:9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolved, err := worker().Resolve(tt.params)
			if err != nil {
				t.Fatalf("Resolve() returned %v, want nil", err)
			}

			if !slices.Equal(resolved.Args, tt.wantArgs) {
				t.Errorf("args = %q, want %q", resolved.Args, tt.wantArgs)
			}

			if resolved.Lock != tt.wantLock {
				t.Errorf("lock = %q, want %q", resolved.Lock, tt.wantLock)
			}
		})
	}
}

func TestWorker_Resolve_rejects(t *testing.T) {
	t.Parallel()

	worker := &Worker{
		Name:    "invoice",
		Command: "/usr/bin/php",
		Args:    []string{"--id={{id}}", "--mode={{mode}}"},
		Params: map[string]Param{
			"id":   {Required: true, Pattern: "^[0-9]{1,12}$"},
			"mode": {Enum: []string{"fast", "slow"}},
		},
	}

	tests := []struct {
		name      string
		params    map[string]string
		wantParam string
	}{
		{
			name:      "undeclared param is refused",
			params:    map[string]string{"id": "1", "shell": "; rm -rf /"},
			wantParam: "shell",
		},
		{
			name:      "required param cannot be omitted",
			params:    map[string]string{},
			wantParam: "id",
		},
		{
			name:      "value must match the declared pattern",
			params:    map[string]string{"id": "123; rm -rf /"},
			wantParam: "id",
		},
		{
			name:      "value must be in the declared enum",
			params:    map[string]string{"id": "1", "mode": "turbo"},
			wantParam: "mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := worker.Resolve(tt.params)
			if err == nil {
				t.Fatal("Resolve() returned nil, want a param error")
			}

			var paramErr *core.ParamError
			if !errors.As(err, &paramErr) {
				t.Fatalf("error is %T, want *core.ParamError", err)
			}

			if paramErr.Param != tt.wantParam {
				t.Errorf("rejected param = %q, want %q", paramErr.Param, tt.wantParam)
			}
		})
	}
}

func TestPlaceholderNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "no placeholder", input: "--verbose", want: []string{}},
		{name: "single placeholder", input: "--id={{id}}", want: []string{"id"}},
		{name: "two placeholders", input: "{{a}}/{{b}}", want: []string{"a", "b"}},
		{name: "malformed braces are literal", input: "{id}", want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := placeholderNames(tt.input); !slices.Equal(got, tt.want) {
				t.Errorf("placeholderNames(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
