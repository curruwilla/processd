// Package sqlite implements store.Store on a local SQLite database.
//
// SQLite serialises writes anyway, so every write goes through a single writer:
// concurrent writers only produce SQLITE_BUSY. Reads use WAL and stay parallel.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds working

	"github.com/curruwilla/processd/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is the SQLite-backed implementation of store.Store.
type Store struct {
	db *sql.DB

	// writeMu serialises writes. See the package comment.
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
	if _, err := tx.ExecContext(ctx, insert, name, formatTime(time.Now())); err != nil {
		return fmt.Errorf("recording migration %q: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration %q: %w", name, err)
	}

	return nil
}

// Ping verifies that the database still answers a trivial query.
//
// A closed or corrupted file is invisible to the HTTP server, which keeps
// answering liveness checks happily; the deep health check needs a read that
// actually reaches the storage layer.
func (s *Store) Ping(ctx context.Context) error {
	var one int

	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("pinging database: %w", err)
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

// formatTime renders a timestamp in the single format the schema stores.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// formatTimePtr renders an optional timestamp, or NULL.
func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}

	return formatTime(*t)
}

func parseTime(raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing timestamp %q: %w", raw, err)
	}

	return parsed, nil
}

func parseTimePtr(raw sql.NullString) (*time.Time, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}

	parsed, err := parseTime(raw.String)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

func encodeJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding column value: %w", err)
	}

	return string(encoded), nil
}

func decodeJSON(raw string, out any) error {
	if raw == "" {
		return nil
	}

	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("decoding column value: %w", err)
	}

	return nil
}

// encodeCursor builds an opaque keyset cursor from the last row of a page.
func encodeCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(formatTime(createdAt) + "|" + id))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("decoding cursor: %w", err)
	}

	timestamp, id, found := strings.Cut(string(raw), "|")
	if !found {
		return time.Time{}, "", errors.New("malformed cursor")
	}

	parsed, err := parseTime(timestamp)
	if err != nil {
		return time.Time{}, "", err
	}

	return parsed, id, nil
}
