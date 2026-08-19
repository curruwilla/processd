package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "processd.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() returned %v, want nil", err)
	}

	var tables int

	row := db.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('processes', 'attempts', 'locks', 'idempotency_keys')`,
	)

	if err := row.Scan(&tables); err != nil {
		t.Fatalf("querying schema: %v", err)
	}

	if tables != 4 {
		t.Errorf("created %d of the expected tables, want 4", tables)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	// Reopening must be a no-op: migrations are applied at most once.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening returned %v, want nil", err)
	}

	if err := reopened.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
}
