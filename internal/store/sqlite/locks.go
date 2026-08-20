package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/curruwilla/processd/internal/core"
)

// AcquireLock claims a lock key for an execution.
//
// The unique key of the locks table is what makes concurrent acquisition
// impossible; re-acquiring a lock the same execution already holds succeeds, so
// a retry never loses the lock it kept across the backoff.
func (s *Store) AcquireLock(ctx context.Context, key, processID string) error {
	if key == "" {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	const insert = `INSERT INTO locks (key, process_id, acquired_at) VALUES (?, ?, ?)`

	_, err := s.db.ExecContext(ctx, insert, key, processID, formatTime(time.Now()))
	if err == nil {
		return nil
	}

	var holder string

	row := s.db.QueryRowContext(ctx, `SELECT process_id FROM locks WHERE key = ?`, key)
	if scanErr := row.Scan(&holder); scanErr != nil {
		return fmt.Errorf("acquiring lock %q: %w", key, err)
	}

	if holder == processID {
		return nil
	}

	return fmt.Errorf("lock %q is held by %s: %w", key, holder, core.ErrLockHeld)
}

// ReleaseLock frees a lock key still held by an execution.
func (s *Store) ReleaseLock(ctx context.Context, key, processID string) error {
	if key == "" {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	const query = `DELETE FROM locks WHERE key = ? AND process_id = ?`

	if _, err := s.db.ExecContext(ctx, query, key, processID); err != nil {
		return fmt.Errorf("releasing lock %q: %w", key, err)
	}

	return nil
}

// ActiveLocks returns the currently held locks, keyed by lock name.
func (s *Store) ActiveLocks(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, process_id FROM locks`)
	if err != nil {
		return nil, fmt.Errorf("reading locks: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()

	locks := map[string]string{}

	for rows.Next() {
		var key, processID string

		if err := rows.Scan(&key, &processID); err != nil {
			return nil, fmt.Errorf("reading locks: %w", err)
		}

		locks[key] = processID
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading locks: %w", err)
	}

	return locks, nil
}
