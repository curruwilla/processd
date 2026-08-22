package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
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

	// Notify is the fallback notification policy. A worker that declares its own
	// replaces it outright rather than merging with it: two half-policies
	// deciding one delivery is harder to read than either of them.
	Notify Notify `yaml:"notify"`

	// Fleet aggregates the read API of other nodes. Only a hub configures it;
	// a node never knows it is being read (docs/SPEC.md §22.3).
	Fleet Fleet `yaml:"fleet"`

	Queue   Queue   `yaml:"queue"`
	History History `yaml:"history"`
	Logs    Logs    `yaml:"logs"`
	UI      UI      `yaml:"ui"`
	Auth    Auth    `yaml:"auth"`
}

// UI configures the built-in web console.
//
// The console is static: it holds no credential of its own and reaches the
// daemon only through the authenticated API, with a token the operator pastes
// into the page. Turning it off removes the routes entirely.
type UI struct {
	Enabled bool `yaml:"enabled"`
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

// Fleet is the read-only aggregation of other nodes.
//
// It is pull-based on purpose: the hub polls, and there is no registration
// protocol and no state on the node side. A node needs no configuration at all
// to be part of a fleet, which is only possible because the aggregation never
// writes.
type Fleet struct {
	Nodes []FleetNode `yaml:"nodes"`

	// PollInterval is how often each node is asked how it is doing. It bounds
	// how stale the dashboard can be, and nothing else: reads are proxied live.
	PollInterval Duration `yaml:"poll_interval"`

	// Timeout bounds one call to one node. A node that stops answering must not
	// hold up the poll of the others, nor a request to the hub.
	Timeout Duration `yaml:"timeout"`
}

// FleetNode is one node the hub reads.
type FleetNode struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`

	// TokenFile holds the read-only token for that node. There is deliberately
	// no inline token: a daemon configuration file stores digests, never
	// secrets, and a fleet is not the place to make an exception.
	TokenFile string `yaml:"token_file"`
}

// IsSet reports whether this daemon aggregates anything.
func (f Fleet) IsSet() bool { return len(f.Nodes) > 0 }

func (f Fleet) validate() []error {
	if !f.IsSet() {
		if f.PollInterval != 0 || f.Timeout != 0 {
			return []error{errors.New("fleet.nodes must not be empty for the other fleet keys to mean anything")}
		}

		return nil
	}

	errs := []error{}
	seen := map[string]bool{}

	for i, node := range f.Nodes {
		errs = append(errs, node.validate(i, seen)...)
	}

	if f.PollInterval <= 0 {
		errs = append(errs, errors.New("fleet.poll_interval must be greater than zero"))
	}

	if f.Timeout <= 0 {
		errs = append(errs, errors.New("fleet.timeout must be greater than zero"))
	}

	return errs
}

// validate checks one node entry, recording its name so a duplicate later in
// the list is reported rather than silently shadowing the first.
func (n FleetNode) validate(index int, seen map[string]bool) []error {
	errs := []error{}

	switch {
	case n.Name == "":
		errs = append(errs, fmt.Errorf("fleet.nodes[%d].name must not be empty", index))
	case seen[n.Name]:
		errs = append(errs, fmt.Errorf("fleet.nodes[%d]: node %q is declared twice", index, n.Name))
	default:
		seen[n.Name] = true
	}

	parsed, err := url.Parse(n.URL)

	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("fleet.nodes[%d].url %q: %w", index, n.URL, err))
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		errs = append(errs, fmt.Errorf("fleet.nodes[%d].url %q must be http or https", index, n.URL))
	case parsed.Host == "":
		errs = append(errs, fmt.Errorf("fleet.nodes[%d].url %q has no host", index, n.URL))
	}

	if !filepath.IsAbs(n.TokenFile) {
		errs = append(errs, fmt.Errorf("fleet.nodes[%d].token_file %q must be an absolute path", index, n.TokenFile))
	}

	return errs
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

	// ReadOnly refuses every state-changing call with this token. It is what a
	// hub uses to aggregate a node it must never write to (docs/SPEC.md §22.3),
	// and what an operator gives to a dashboard.
	ReadOnly bool `yaml:"read_only"`
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
		UI:   UI{Enabled: true},
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

	// The daemon-wide policy gets the same bounds a worker's does: an operator
	// should not have to restate "do not hang" depending on where it is written.
	applyNotifyDefaults(&cfg.Notify)
	applyFleetDefaults(&cfg.Fleet)

	return cfg, cfg.Validate()
}

// applyFleetDefaults gives the aggregation the bounds it must have, so that an
// operator listing nodes does not also have to remember to bound the polling.
func applyFleetDefaults(f *Fleet) {
	if !f.IsSet() {
		return
	}

	if f.PollInterval == 0 {
		f.PollInterval = Duration(10 * time.Second)
	}

	if f.Timeout == 0 {
		f.Timeout = Duration(5 * time.Second)
	}
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

	errs = append(errs, c.Notify.validate()...)
	errs = append(errs, c.Fleet.validate()...)

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
