package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curruwilla/processd/internal/api"
	"github.com/curruwilla/processd/internal/config"
)

// seedConfig writes a configuration whose directories all live inside the test
// directory, so that setup never touches /var or /etc.
func seedConfig(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "processd.yaml")
	content := strings.Join([]string{
		"listen: 127.0.0.1:7373",
		"data_dir: " + filepath.Join(dir, "data"),
		"log_dir: " + filepath.Join(dir, "logs"),
		"workers_dir: " + filepath.Join(dir, "workers.d"),
		"",
	}, "\n")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the seed configuration: %v", err)
	}

	return path
}

// localOptions installs into the test directory and leaves systemd alone.
func localOptions(configPath string) Options {
	return Options{ConfigPath: configPath}
}

func TestRunInstallsNode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	result, err := Run(t.Context(), localOptions(path))
	if err != nil {
		t.Fatalf("Run() returned %v, want nil", err)
	}

	if result.Token == "" || result.TokenReused {
		t.Fatalf("Run() token = %q, reused = %v, want a fresh token", result.Token, result.TokenReused)
	}

	// The daemon must accept exactly the token that was printed.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reloading the configuration: %v", err)
	}

	if len(cfg.Auth.Tokens) != 1 || cfg.Auth.Tokens[0].Hash != api.HashToken(result.Token) {
		t.Fatalf("configuration tokens = %+v, want the digest of %q", cfg.Auth.Tokens, result.Token)
	}

	if cfg.Auth.Tokens[0].Name != DefaultTokenName {
		t.Errorf("token name = %q, want %q", cfg.Auth.Tokens[0].Name, DefaultTokenName)
	}

	info, err := os.Stat(result.TokenPath)
	if err != nil {
		t.Fatalf("stating the token file: %v", err)
	}

	if perm := info.Mode().Perm(); perm != tokenPerm {
		t.Errorf("token file mode = %v, want %v", perm, tokenPerm)
	}

	for _, dir := range []string{result.DataDir, result.LogDir, result.WorkersDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("directory %s was not created: %v", dir, err)
		}
	}

	if result.BaseURL != "http://127.0.0.1:7373" || result.UIURL != "http://127.0.0.1:7373/ui/" {
		t.Errorf("addresses = %q and %q, want the loopback API and UI", result.BaseURL, result.UIURL)
	}
}

func TestRunKeepsTheInstalledToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	first, err := Run(t.Context(), localOptions(path))
	if err != nil {
		t.Fatalf("first Run() returned %v, want nil", err)
	}

	second, err := Run(t.Context(), localOptions(path))
	if err != nil {
		t.Fatalf("second Run() returned %v, want nil", err)
	}

	if second.Token != first.Token || !second.TokenReused {
		t.Fatalf("second Run() token = %q (reused %v), want the first token %q", second.Token, second.TokenReused, first.Token)
	}

	// Nothing changed, so there is nothing to back up either.
	if second.ConfigBackup != "" {
		t.Errorf("second Run() backed up the configuration to %q, want no backup", second.ConfigBackup)
	}
}

func TestRunRotatesOnRequest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	first, err := Run(t.Context(), localOptions(path))
	if err != nil {
		t.Fatalf("first Run() returned %v, want nil", err)
	}

	opts := localOptions(path)
	opts.Rotate = true

	second, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("second Run() returned %v, want nil", err)
	}

	if second.Token == first.Token {
		t.Fatal("Run(Rotate) kept the previous token, want a new one")
	}

	if second.ConfigBackup == "" {
		t.Error("Run(Rotate) rewrote the configuration without saving a backup")
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reloading the configuration: %v", err)
	}

	if len(cfg.Auth.Tokens) != 1 || cfg.Auth.Tokens[0].Hash != api.HashToken(second.Token) {
		t.Fatalf("configuration tokens = %+v, want only the rotated digest", cfg.Auth.Tokens)
	}
}

// A node whose token file was lost must become usable again, instead of leaving
// the operator with a digest nobody holds the secret for.
func TestRunReplacesALostToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	first, err := Run(t.Context(), localOptions(path))
	if err != nil {
		t.Fatalf("first Run() returned %v, want nil", err)
	}

	if err := os.Remove(first.TokenPath); err != nil {
		t.Fatalf("removing the token file: %v", err)
	}

	second, err := Run(t.Context(), localOptions(path))
	if err != nil {
		t.Fatalf("second Run() returned %v, want nil", err)
	}

	if second.Token == first.Token || second.TokenReused {
		t.Fatal("Run() reported a reused token although the token file was gone")
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reloading the configuration: %v", err)
	}

	if cfg.Auth.Tokens[0].Hash != api.HashToken(second.Token) {
		t.Error("the configuration still holds the digest of the lost token")
	}
}

func TestRunPreservesOtherTokens(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	extra := "\nauth:\n  tokens:\n    - name: billing-cron\n      hash: \"" + api.HashToken("other") + "\"\n      workers: [\"invoice\"]\n"

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening the seed configuration: %v", err)
	}

	if _, err := file.WriteString(extra); err != nil {
		t.Fatalf("extending the seed configuration: %v", err)
	}

	if err := file.Close(); err != nil {
		t.Fatalf("closing the seed configuration: %v", err)
	}

	if _, err := Run(t.Context(), localOptions(path)); err != nil {
		t.Fatalf("Run() returned %v, want nil", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reloading the configuration: %v", err)
	}

	if len(cfg.Auth.Tokens) != 2 {
		t.Fatalf("configuration tokens = %+v, want the existing token plus the managed one", cfg.Auth.Tokens)
	}

	if cfg.Auth.Tokens[0].Name != "billing-cron" || len(cfg.Auth.Tokens[0].Workers) != 1 {
		t.Errorf("token %+v lost its scope", cfg.Auth.Tokens[0])
	}
}

func TestRunDryRunChangesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the seed configuration: %v", err)
	}

	opts := localOptions(path)
	opts.DryRun = true

	result, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() returned %v, want nil", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the configuration back: %v", err)
	}

	if string(after) != string(before) {
		t.Error("the dry run rewrote the configuration")
	}

	if _, err := os.Stat(result.TokenPath); err == nil {
		t.Error("the dry run wrote the token file")
	}

	if _, err := os.Stat(result.DataDir); err == nil {
		t.Error("the dry run created the data directory")
	}

	if len(result.Steps) == 0 {
		t.Error("the dry run reported no planned step")
	}
}

func TestRunInstallsTheSystemdUnit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	calls := [][]string{}

	opts := localOptions(path)
	opts.Binary = "/usr/local/bin/processd"
	opts.UnitPath = filepath.Join(dir, "processd.service")
	opts.InstallUnit = true
	opts.StartUnit = true
	opts.Systemctl = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))

		return nil
	}

	result, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() returned %v, want nil", err)
	}

	unit, err := os.ReadFile(opts.UnitPath)
	if err != nil {
		t.Fatalf("reading the unit: %v", err)
	}

	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/processd serve --config "+path) {
		t.Errorf("unit ExecStart does not start this installation:\n%s", unit)
	}

	// shutdown_grace defaults to 30s, and the unit must outlast it.
	if !strings.Contains(string(unit), "TimeoutStopSec=45s") {
		t.Errorf("unit TimeoutStopSec does not cover shutdown_grace:\n%s", unit)
	}

	want := [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "processd"}}
	if len(calls) != len(want) {
		t.Fatalf("systemctl calls = %v, want %v", calls, want)
	}

	for i, call := range calls {
		if strings.Join(call, " ") != strings.Join(want[i], " ") {
			t.Errorf("systemctl call %d = %v, want %v", i, call, want[i])
		}
	}

	if result.ServiceState != "started" || len(result.Warnings) != 0 {
		t.Errorf("service state = %q with warnings %v, want a clean start", result.ServiceState, result.Warnings)
	}
}

func TestRunWarnsAboutABinaryOutsideTheSystemPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedConfig(t, dir)

	opts := localOptions(path)
	opts.Binary = filepath.Join(dir, "bin", "processd")
	opts.UnitPath = filepath.Join(dir, "processd.service")
	opts.InstallUnit = true
	opts.Systemctl = func(context.Context, string, ...string) error { return nil }

	result, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() returned %v, want nil", err)
	}

	if len(result.Warnings) == 0 {
		t.Error("Run() installed a unit pointing at a build tree without warning")
	}
}

func TestBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		listen string
		want   string
	}{
		{name: "loopback", listen: "127.0.0.1:7373", want: "http://127.0.0.1:7373"},
		{name: "wildcard host", listen: ":7373", want: "http://127.0.0.1:7373"},
		{name: "any ipv4", listen: "0.0.0.0:7373", want: "http://127.0.0.1:7373"},
		{name: "any ipv6", listen: "[::]:7373", want: "http://127.0.0.1:7373"},
		{name: "explicit ipv6", listen: "[::1]:7373", want: "http://[::1]:7373"},
		{name: "named host", listen: "processd.internal:8080", want: "http://processd.internal:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := baseURL(tt.listen); got != tt.want {
				t.Errorf("baseURL(%q) = %q, want %q", tt.listen, got, tt.want)
			}
		})
	}
}

func TestGenerateTokenIsUnique(t *testing.T) {
	t.Parallel()

	first, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() returned %v, want nil", err)
	}

	second, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() returned %v, want nil", err)
	}

	if len(first) != tokenBytes*2 {
		t.Errorf("token length = %d, want %d hex characters", len(first), tokenBytes*2)
	}

	if first == second {
		t.Error("GenerateToken() returned the same token twice")
	}
}

func TestTokenPath(t *testing.T) {
	t.Parallel()

	if got := TokenPath("/etc/processd/processd.yaml"); got != "/etc/processd/token" {
		t.Errorf("TokenPath() = %q, want /etc/processd/token", got)
	}

	if got := TokenPath(""); got != "/etc/processd/token" {
		t.Errorf("TokenPath(\"\") = %q, want the default location", got)
	}
}

func TestReadTokenWithoutAFile(t *testing.T) {
	t.Parallel()

	token, err := ReadToken(filepath.Join(t.TempDir(), "processd.yaml"))
	if err != nil {
		t.Fatalf("ReadToken() returned %v, want nil", err)
	}

	if token != "" {
		t.Errorf("ReadToken() = %q, want an empty token", token)
	}
}
