package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// ExecutionMode decides how much of the command line a client may choose.
type ExecutionMode string

const (
	// ExecutionModeWorkers only runs pre-configured workers. Default, and the
	// only mode that makes the API safe to expose to ordinary clients.
	ExecutionModeWorkers ExecutionMode = "workers"
	// ExecutionModeRaw allows a client-supplied command, restricted to an
	// explicit allowlist. Deliberately insecure: a free command is RCE.
	ExecutionModeRaw ExecutionMode = "raw"
)

// OrphanPolicy decides what happens to a process that outlived the daemon.
type OrphanPolicy string

const (
	// OrphanPolicyKill terminates the orphan before retrying. Default: leaving
	// it alive while a retry starts would run the same work twice.
	OrphanPolicyKill OrphanPolicy = "kill"
	// OrphanPolicyLeave records the orphan for manual inspection and does not
	// retry the execution.
	OrphanPolicyLeave OrphanPolicy = "leave"
)

// Config is the daemon configuration (docs/SPEC.md §20).
type Config struct {
	Listen     string `yaml:"listen"`
	DataDir    string `yaml:"data_dir"`
	LogDir     string `yaml:"log_dir"`
	WorkersDir string `yaml:"workers_dir"`

	MaxProcesses  int          `yaml:"max_processes"`
	ShutdownGrace Duration     `yaml:"shutdown_grace"`
	OrphanPolicy  OrphanPolicy `yaml:"orphan_policy"`

	ExecutionMode      ExecutionMode `yaml:"execution_mode"`
	AllowedCommands    []string      `yaml:"allowed_commands"`
	AllowRootProcesses bool          `yaml:"allow_root_processes"`

	Queue   Queue   `yaml:"queue"`
	History History `yaml:"history"`
	Logs    Logs    `yaml:"logs"`
	Auth    Auth    `yaml:"auth"`
}

// Queue bounds the admission queue. An unbounded queue is a memory leak.
type Queue struct {
	MaxDepth int      `yaml:"max_depth"`
	ItemTTL  Duration `yaml:"item_ttl"`
}

// History bounds how much finished-execution state is kept in SQLite.
type History struct {
	Retention Duration `yaml:"retention"`
	MaxRows   int      `yaml:"max_rows"`
}

// Logs bounds captured output. Without a cap, a process that logs in a loop
// fills the disk.
type Logs struct {
	MaxBytesPerStream ByteSize `yaml:"max_bytes_per_stream"`
	Retention         Duration `yaml:"retention"`
}

// Auth holds the API tokens accepted by the daemon.
type Auth struct {
	Tokens []Token `yaml:"tokens"`
}

// Token is an API credential. Only the hash is stored; the secret itself never
// touches the configuration file.
type Token struct {
	Name    string   `yaml:"name"`
	Hash    string   `yaml:"hash"`
	Workers []string `yaml:"workers"`
}

// AllowsWorker reports whether the token may act on the named worker. An empty
// list grants access to every worker.
func (t Token) AllowsWorker(name string) bool {
	if len(t.Workers) == 0 {
		return true
	}

	return slices.Contains(t.Workers, name)
}

// Default returns the configuration used when no file is present. The defaults
// are deliberately restrictive: loopback bind, workers-only execution, no root
// processes.
func Default() Config {
	return Config{
		Listen:             "127.0.0.1:7373",
		DataDir:            "/var/lib/processd",
		LogDir:             "/var/log/processd",
		WorkersDir:         "/etc/processd/workers.d",
		MaxProcesses:       50,
		ShutdownGrace:      Duration(30 * time.Second),
		OrphanPolicy:       OrphanPolicyKill,
		ExecutionMode:      ExecutionModeWorkers,
		AllowedCommands:    []string{},
		AllowRootProcesses: false,
		Queue: Queue{
			MaxDepth: 1000,
			ItemTTL:  Duration(time.Hour),
		},
		History: History{
			Retention: Duration(30 * 24 * time.Hour),
			MaxRows:   500_000,
		},
		Logs: Logs{
			MaxBytesPerStream: 32 << 20,
			Retention:         Duration(14 * 24 * time.Hour),
		},
		Auth: Auth{Tokens: []Token{}},
	}
}

// Load reads the daemon configuration from path, applying defaults for every
// key the file omits. A missing file is not an error: the defaults are usable.
func Load(path string) (Config, error) {
	cfg := Default()

	file, err := os.Open(path) //nolint:gosec // the path is chosen by the operator, not by a client
	if errors.Is(err, os.ErrNotExist) {
		return cfg, cfg.Validate()
	}

	if err != nil {
		return cfg, fmt.Errorf("opening config %q: %w", path, err)
	}

	defer func() {
		_ = file.Close() // read-only handle
	}()

	if err := decodeStrict(file, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %q: %w", path, err)
	}

	return cfg, cfg.Validate()
}

// Validate rejects configurations that would make the daemon unsafe or
// unable to make progress.
func (c Config) Validate() error {
	errs := []error{}

	if c.Listen == "" {
		errs = append(errs, errors.New("listen must not be empty"))
	}

	if c.MaxProcesses <= 0 {
		errs = append(errs, errors.New("max_processes must be greater than zero"))
	}

	if c.Queue.MaxDepth <= 0 {
		errs = append(errs, errors.New("queue.max_depth must be greater than zero"))
	}

	if c.Logs.MaxBytesPerStream <= 0 {
		errs = append(errs, errors.New("logs.max_bytes_per_stream must be greater than zero"))
	}

	switch c.OrphanPolicy {
	case OrphanPolicyKill, OrphanPolicyLeave:
	default:
		errs = append(errs, fmt.Errorf("orphan_policy %q is unknown", c.OrphanPolicy))
	}

	errs = append(errs, c.validateExecution()...)

	for i, token := range c.Auth.Tokens {
		if token.Name == "" || token.Hash == "" {
			errs = append(errs, fmt.Errorf("auth.tokens[%d] needs both name and hash", i))
		}
	}

	return errors.Join(errs...)
}

func (c Config) validateExecution() []error {
	errs := []error{}

	switch c.ExecutionMode {
	case ExecutionModeWorkers:
	case ExecutionModeRaw:
		if len(c.AllowedCommands) == 0 {
			errs = append(errs, errors.New("execution_mode raw requires a non-empty allowed_commands list"))
		}
	default:
		errs = append(errs, fmt.Errorf("execution_mode %q is unknown", c.ExecutionMode))
	}

	for _, command := range c.AllowedCommands {
		if !filepath.IsAbs(command) {
			errs = append(errs, fmt.Errorf("allowed_commands entry %q must be an absolute path", command))
		}
	}

	return errs
}

// decodeStrict decodes YAML and rejects unknown fields.
func decodeStrict(r io.Reader, out any) error {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	if err := decoder.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}
