// Package sqlite implements store.Store on a local SQLite database.
//
// SQLite serialises writes anyway, so every write goes through a single writer:
// concurrent writers only produce SQLITE_BUSY. Reads use WAL and stay parallel.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds working

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// errNotImplemented marks the parts of the skeleton still to be written.
var errNotImplemented = errors.New("not implemented")

// Store is the SQLite-backed implementation of store.Store.
type Store struct {
	db *sql.DB

	// writeMu serialises writes. See the package comment.
	//nolint:unused // taken by the write methods listed at the bottom of this file
	writeMu sync.Mutex
}

var _ store.Store = (*Store)(nil)

// Open opens (and creates, if needed) the database at path and applies every
// pending migration.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		filepath.Clean(path),
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database %q: %w", path, err)
	}

	s := &Store{db: db}

	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// migrate applies the embedded migrations in filename order, once each.
func (s *Store) migrate(ctx context.Context) error {
	const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`

	if _, err := s.db.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	sort.Strings(names)

	for _, name := range names {
		if err := s.applyMigration(ctx, name); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) applyMigration(ctx context.Context, name string) error {
	var applied int

	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name)
	if err := row.Scan(&applied); err != nil {
		return fmt.Errorf("checking migration %q: %w", name, err)
	}

	if applied > 0 {
		return nil
	}

	statements, err := migrationFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("reading migration %q: %w", name, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting migration %q: %w", name, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, string(statements)); err != nil {
		return fmt.Errorf("applying migration %q: %w", name, err)
	}

	insert := `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`
	if _, err := tx.ExecContext(ctx, insert, name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("recording migration %q: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration %q: %w", name, err)
	}

	return nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing database: %w", err)
	}

	return nil
}

// TODO(spec §17): the methods below are the remaining persistence work. Each
// write must take writeMu; reads must not.

// CreateProcess persists a newly submitted execution.
func (s *Store) CreateProcess(ctx context.Context, p *core.Process) error {
	return errNotImplemented
}

// UpdateProcess persists a state change of an execution.
func (s *Store) UpdateProcess(ctx context.Context, p *core.Process) error {
	return errNotImplemented
}

// GetProcess returns one execution by its logical ID.
func (s *Store) GetProcess(ctx context.Context, id string) (*core.Process, error) {
	return nil, errNotImplemented
}

// ListProcesses returns one cursor-paginated page of executions.
func (s *Store) ListProcesses(ctx context.Context, f store.Filter) (store.Page, error) {
	return store.Page{}, errNotImplemented
}

// UnfinishedProcesses returns the executions left in a non-terminal state,
// used by the startup reconciliation pass.
func (s *Store) UnfinishedProcesses(ctx context.Context) ([]*core.Process, error) {
	return nil, errNotImplemented
}

// AcquireLock claims a lock key for an execution.
func (s *Store) AcquireLock(ctx context.Context, key, processID string) error {
	return errNotImplemented
}

// ReleaseLock frees a lock key still held by an execution.
func (s *Store) ReleaseLock(ctx context.Context, key, processID string) error {
	return errNotImplemented
}

// ActiveLocks returns the currently held locks, keyed by lock name.
func (s *Store) ActiveLocks(ctx context.Context) (map[string]string, error) {
	return nil, errNotImplemented
}

// SaveIdempotency records the outcome of an idempotent request.
func (s *Store) SaveIdempotency(ctx context.Context, record store.Idempotency) error {
	return errNotImplemented
}

// FindIdempotency returns a previously recorded idempotent request.
func (s *Store) FindIdempotency(ctx context.Context, key string) (store.Idempotency, error) {
	return store.Idempotency{}, errNotImplemented
}

// PurgeHistory removes terminal executions beyond the retention limits.
func (s *Store) PurgeHistory(ctx context.Context, before time.Time, maxRows int) (int, error) {
	return 0, errNotImplemented
}
