// Package setup installs a processd node in a single step: the state
// directories, the daemon configuration, an API token and the systemd unit.
//
// The daemon stores only the digest of a token, which makes a lost secret
// unrecoverable. Setup therefore keeps the plaintext in a root-only file next
// to the configuration, so that a second run — and every CLI command on the
// node — reuses the same token instead of minting one nobody wrote down.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/curruwilla/processd/internal/api"
	"github.com/curruwilla/processd/internal/config"
)

// File modes. The configuration and the token file are secrets at rest: both
// carry credentials for the API, so nothing outside the owner may read them.
const (
	configPerm fs.FileMode = 0o600
	tokenPerm  fs.FileMode = 0o600
	dirPerm    fs.FileMode = 0o750
)

// Defaults for the pieces the operator does not name.
const (
	// DefaultConfigPath is where the daemon looks for its configuration.
	DefaultConfigPath = "/etc/processd/processd.yaml"
	// DefaultTokenName labels the token setup manages in the audit trail.
	DefaultTokenName = "admin"
	// DefaultService is the systemd unit name.
	DefaultService = "processd"
	// DefaultUnitDir is where the generated unit is installed.
	DefaultUnitDir = "/etc/systemd/system"
	// DefaultBinary is the ExecStart path used when the running binary cannot
	// be resolved.
	DefaultBinary = "/usr/local/bin/processd"
)

// Runner executes an external command. Setup only uses it for systemctl; tests
// substitute it to record the invocations instead of running them.
type Runner func(ctx context.Context, name string, args ...string) error

// Options describes the installation. The zero value is usable: withDefaults
// fills every field the operator left empty.
type Options struct {
	ConfigPath string
	// Listen overrides listen in the configuration. Empty keeps the value the
	// file already has, or the default for a fresh install.
	Listen    string
	TokenName string
	// Binary is the ExecStart path of the unit. Empty resolves the running
	// executable.
	Binary   string
	UnitPath string
	Service  string
	// InstallUnit writes the systemd unit and reloads systemd.
	InstallUnit bool
	// StartUnit enables and starts the service. Ignored without InstallUnit.
	StartUnit bool
	// Rotate mints a new token even when the current one still works.
	Rotate bool
	// DryRun reports what would happen without touching the filesystem.
	DryRun bool
	// Systemctl runs systemctl. A non-nil runner also skips the probe for a
	// live systemd, so that tests do not depend on the host init system.
	Systemctl Runner
}

// Result is what setup did, and everything an operator needs afterwards.
type Result struct {
	ConfigPath   string `json:"config_path"`
	ConfigBackup string `json:"config_backup,omitempty"`
	TokenPath    string `json:"token_path"`
	TokenName    string `json:"token_name"`
	Token        string `json:"token"`
	TokenReused  bool   `json:"token_reused"`

	WorkersDir string `json:"workers_dir"`
	DataDir    string `json:"data_dir"`
	LogDir     string `json:"log_dir"`

	Listen     string `json:"listen"`
	BaseURL    string `json:"base_url"`
	UIURL      string `json:"ui_url,omitempty"`
	MetricsURL string `json:"metrics_url"`

	UnitPath     string `json:"unit_path,omitempty"`
	Service      string `json:"service,omitempty"`
	ServiceState string `json:"service_state"`

	Steps    []string `json:"steps"`
	Warnings []string `json:"warnings,omitempty"`
	DryRun   bool     `json:"dry_run"`
}

// TokenPath returns the plaintext token file that belongs to a configuration.
func TokenPath(configPath string) string {
	if configPath == "" {
		configPath = DefaultConfigPath
	}

	return filepath.Join(filepath.Dir(configPath), "token")
}

// ReadToken returns the plaintext token stored next to the configuration. A
// missing file yields an empty token and no error: not every node is set up
// with this command.
func ReadToken(configPath string) (string, error) {
	path := TokenPath(configPath)

	raw, err := os.ReadFile(path) //nolint:gosec // the path derives from the operator's own --config flag
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}

	return strings.TrimSpace(string(raw)), nil
}

// Run performs the installation and reports what it did.
func Run(ctx context.Context, opts Options) (Result, error) {
	opts = opts.withDefaults()

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return Result{}, fmt.Errorf("loading %s: %w", opts.ConfigPath, err)
	}

	if opts.Listen != "" {
		cfg.Listen = opts.Listen
	}

	res := Result{
		ConfigPath: opts.ConfigPath,
		TokenPath:  TokenPath(opts.ConfigPath),
		TokenName:  opts.TokenName,
		DryRun:     opts.DryRun,
	}

	token, reused, err := resolveToken(opts, cfg)
	if err != nil {
		return res, err
	}

	res.Token, res.TokenReused = token, reused
	cfg.Auth.Tokens = upsertToken(cfg.Auth.Tokens, opts.TokenName, api.HashToken(token))

	// A configuration setup itself wrote must never be one the daemon refuses.
	if err := cfg.Validate(); err != nil {
		return res, fmt.Errorf("configuration is invalid after setup: %w", err)
	}

	for _, step := range []func(config.Config, *Result) error{opts.ensureDirs, opts.writeConfig, opts.writeToken} {
		if err := step(cfg, &res); err != nil {
			return res, err
		}
	}

	if err := opts.installService(ctx, cfg, &res); err != nil {
		return res, err
	}

	res.describe(cfg)

	return res, nil
}

func (o Options) withDefaults() Options {
	if o.ConfigPath == "" {
		o.ConfigPath = DefaultConfigPath
	}

	if o.TokenName == "" {
		o.TokenName = DefaultTokenName
	}

	if o.Service == "" {
		o.Service = DefaultService
	}

	if o.Binary == "" {
		o.Binary = currentBinary()
	}

	if o.UnitPath == "" {
		o.UnitPath = filepath.Join(DefaultUnitDir, o.Service+".service")
	}

	return o
}

// currentBinary resolves the running executable, so that the unit starts the
// same build the operator just invoked.
func currentBinary() string {
	path, err := os.Executable()
	if err != nil {
		return DefaultBinary
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}

	return resolved
}

// resolveToken keeps the token already installed on the node when it still
// matches the configuration, and mints a new one otherwise. Reuse is the point
// of the token file: rerunning setup must not invalidate the credential every
// client on the node is already using.
func resolveToken(opts Options, cfg config.Config) (string, bool, error) {
	if !opts.Rotate {
		existing, err := ReadToken(opts.ConfigPath)
		if err != nil {
			return "", false, err
		}

		if existing != "" && tokenMatches(cfg, opts.TokenName, existing) {
			return existing, true, nil
		}
	}

	token, err := GenerateToken()
	if err != nil {
		return "", false, err
	}

	return token, false, nil
}

// tokenMatches reports whether the named configuration entry already holds the
// digest of this secret.
func tokenMatches(cfg config.Config, name, token string) bool {
	digest := api.HashToken(token)

	for _, entry := range cfg.Auth.Tokens {
		if entry.Name == name {
			return entry.Hash == digest
		}
	}

	return false
}

// upsertToken replaces the digest of the named token, preserving its worker
// scope, and leaves every other token untouched.
func upsertToken(tokens []config.Token, name, hash string) []config.Token {
	for i, entry := range tokens {
		if entry.Name == name {
			tokens[i].Hash = hash

			return tokens
		}
	}

	return append(tokens, config.Token{Name: name, Hash: hash})
}

func (o Options) ensureDirs(cfg config.Config, res *Result) error {
	dirs := []string{filepath.Dir(o.ConfigPath), cfg.WorkersDir, cfg.DataDir, cfg.LogDir}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}

		if _, err := os.Stat(dir); err == nil {
			continue
		}

		if o.DryRun {
			res.step("would create " + dir)

			continue
		}

		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return permissionHint(fmt.Errorf("creating %s: %w", dir, err))
		}

		res.step("created " + dir)
	}

	return nil
}

func (o Options) writeConfig(cfg config.Config, res *Result) error {
	rendered, err := renderConfig(cfg)
	if err != nil {
		return err
	}

	current, err := os.ReadFile(o.ConfigPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return permissionHint(fmt.Errorf("reading %s: %w", o.ConfigPath, err))
	}

	if bytes.Equal(current, rendered) {
		res.step("configuration already current: " + o.ConfigPath)

		return nil
	}

	if o.DryRun {
		res.step("would write " + o.ConfigPath)

		return nil
	}

	// Rendering from the parsed configuration drops comments and reorders keys,
	// so an operator who hand-edited the file keeps a copy of what was there.
	if len(current) > 0 {
		backup := o.ConfigPath + ".bak-" + time.Now().UTC().Format("20060102T150405Z")

		//nolint:gosec // the backup sits beside the configuration the operator named
		if err := os.WriteFile(backup, current, configPerm); err != nil {
			return permissionHint(fmt.Errorf("writing %s: %w", backup, err))
		}

		res.ConfigBackup = backup
		res.step("saved the previous configuration to " + backup)
	}

	if err := os.WriteFile(o.ConfigPath, rendered, configPerm); err != nil {
		return permissionHint(fmt.Errorf("writing %s: %w", o.ConfigPath, err))
	}

	res.step("wrote " + o.ConfigPath)

	return nil
}

func (o Options) writeToken(_ config.Config, res *Result) error {
	path := TokenPath(o.ConfigPath)

	if o.DryRun {
		res.step("would write " + path)

		return nil
	}

	if err := os.WriteFile(path, []byte(res.Token+"\n"), tokenPerm); err != nil {
		return permissionHint(fmt.Errorf("writing %s: %w", path, err))
	}

	// A file created before this run keeps whatever mode it had.
	if err := os.Chmod(path, tokenPerm); err != nil {
		return permissionHint(fmt.Errorf("securing %s: %w", path, err))
	}

	if res.TokenReused {
		res.step("kept the existing token in " + path)
	} else {
		res.step("wrote a new token to " + path)
	}

	return nil
}

// renderConfig serialises the configuration the daemon will read back.
func renderConfig(cfg config.Config) ([]byte, error) {
	buf := &bytes.Buffer{}

	buf.WriteString("# Written by `processd setup`. See docs/SPEC.md §20 for every key.\n")
	buf.WriteString("# auth.tokens[].hash is a digest: the secret lives in the token file\n")
	buf.WriteString("# next to this one, and cannot be recovered from here.\n")

	encoder := yaml.NewEncoder(buf)
	encoder.SetIndent(2)

	if err := encoder.Encode(cfg); err != nil {
		return nil, fmt.Errorf("rendering configuration: %w", err)
	}

	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("rendering configuration: %w", err)
	}

	return buf.Bytes(), nil
}

// describe fills in the addresses and paths the operator needs for any later
// check.
func (r *Result) describe(cfg config.Config) {
	r.Listen = cfg.Listen
	r.DataDir = cfg.DataDir
	r.LogDir = cfg.LogDir
	r.WorkersDir = cfg.WorkersDir
	r.BaseURL = baseURL(cfg.Listen)
	r.MetricsURL = r.BaseURL + "/v1/metrics"

	if cfg.UI.Enabled {
		r.UIURL = r.BaseURL + "/ui/"
	}
}

// baseURL turns a listen address into a URL a client can call. A wildcard or
// empty host becomes the loopback address, which is where a local check goes.
func baseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}

	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port)
}

func (r *Result) step(message string) {
	r.Steps = append(r.Steps, message)
}

func (r *Result) warn(message string) {
	r.Warnings = append(r.Warnings, message)
}

// permissionHint names the usual cause: setup writes under /etc and talks to
// systemd, and neither works without root.
func permissionHint(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w (run it as root: sudo processd setup)", err)
	}

	return err
}
