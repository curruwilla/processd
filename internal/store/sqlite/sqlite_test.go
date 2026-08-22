package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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

// TestFormatTime_sorts pins the property the schema depends on: these
// timestamps are text, SQLite compares text byte by byte, so the format has to
// order the way the instants do.
func TestFormatTime_sorts(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

	// Every one of these breaks under a format that strips trailing zeros.
	instants := []time.Time{
		base,
		base.Add(50 * time.Millisecond),
		base.Add(500 * time.Millisecond),
		base.Add(500*time.Millisecond + time.Nanosecond),
		base.Add(999999999 * time.Nanosecond),
		base.Add(time.Second),
	}

	for i := 1; i < len(instants); i++ {
		earlier, later := formatTime(instants[i-1]), formatTime(instants[i])

		if earlier >= later {
			t.Errorf("%q does not sort before %q, but %s is before %s",
				earlier, later, instants[i-1], instants[i])
		}

		if len(earlier) != len(later) {
			t.Errorf("%q and %q have different widths, so text comparison is not ordering",
				earlier, later)
		}
	}

	// It stays a timestamp: what is written has to read back unchanged.
	for _, instant := range instants {
		parsed, err := parseTime(formatTime(instant))
		if err != nil {
			t.Fatalf("parseTime(%q) returned %v, want nil", formatTime(instant), err)
		}

		if !parsed.Equal(instant) {
			t.Errorf("round trip of %s produced %s", instant, parsed)
		}
	}
}

// A database written before the format was fixed holds timestamps that do not
// sort. Leaving them would mean comparing two shapes against each other, so the
// migration rewrites them.
func TestOpen_normalisesStoredTimestamps(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "processd.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() returned %v, want nil", err)
	}

	ctx := t.Context()

	// Write the pre-migration shapes directly, as an older daemon would have.
	legacy := []struct {
		id        string
		createdAt string
		want      string
	}{
		{id: "proc_bare", createdAt: "2026-08-22T10:00:00Z", want: "2026-08-22T10:00:00.000000000Z"},
		{id: "proc_short", createdAt: "2026-08-22T10:00:00.5Z", want: "2026-08-22T10:00:00.500000000Z"},
		{id: "proc_full", createdAt: "2026-08-22T10:00:00.123456789Z", want: "2026-08-22T10:00:00.123456789Z"},
	}

	const insert = `INSERT INTO processes (id, worker, type, state, command, args, env, cwd, created_at)
		VALUES (?, 'invoice', 'task', 'COMPLETED', '/bin/true', '[]', '{}', '/', ?)`

	for _, row := range legacy {
		if _, err := db.db.ExecContext(ctx, insert, row.id, row.createdAt); err != nil {
			t.Fatalf("seeding %s: %v", row.id, err)
		}
	}

	// Force the normalising pass to run again over the rows just written.
	if _, err := db.db.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE name = '0004_sortable_timestamps.sql'`); err != nil {
		t.Fatalf("rewinding the migration: %v", err)
	}

	if err := db.migrate(ctx); err != nil {
		t.Fatalf("migrate() returned %v, want nil", err)
	}

	for _, row := range legacy {
		var got string

		if err := db.db.QueryRowContext(ctx,
			`SELECT created_at FROM processes WHERE id = ?`, row.id).Scan(&got); err != nil {
			t.Fatalf("reading %s: %v", row.id, err)
		}

		if got != row.want {
			t.Errorf("%s: created_at = %q, want %q", row.id, got, row.want)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
}
