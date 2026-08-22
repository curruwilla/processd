package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

// SaveIdempotency claims an idempotency key for an execution.
//
// The primary key of the table is what makes the claim exclusive, exactly as it
// is for a lock: two copies of the same request arriving together cannot both
// win it, so they cannot both start the work. Re-claiming a key the same
// execution already holds succeeds.
func (s *Store) SaveIdempotency(ctx context.Context, record store.Idempotency) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	const insert = `INSERT INTO idempotency_keys (key, request_hash, process_id, created_at)
		VALUES (?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, insert,
		record.Key, record.RequestHash, record.ProcessID, formatTime(record.CreatedAt),
	)
	if err == nil {
		return nil
	}

	var holder string

	row := s.db.QueryRowContext(ctx, `SELECT process_id FROM idempotency_keys WHERE key = ?`, record.Key)
	if scanErr := row.Scan(&holder); scanErr != nil {
		return fmt.Errorf("saving idempotency key: %w", err)
	}

	if holder == record.ProcessID {
		return nil
	}

	return fmt.Errorf("key %q is claimed by %s: %w", record.Key, holder, core.ErrIdempotencyClaimed)
}

// DeleteIdempotency releases a claim whose execution never started, so that a
// request refused before it ran can be repeated with the same key.
func (s *Store) DeleteIdempotency(ctx context.Context, key, processID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	const query = `DELETE FROM idempotency_keys WHERE key = ? AND process_id = ?`

	if _, err := s.db.ExecContext(ctx, query, key, processID); err != nil {
		return fmt.Errorf("releasing idempotency key: %w", err)
	}

	return nil
}

// FindIdempotency returns a previously recorded key, or core.ErrNotFound.
func (s *Store) FindIdempotency(ctx context.Context, key string) (store.Idempotency, error) {
	const query = `SELECT key, request_hash, process_id, created_at FROM idempotency_keys WHERE key = ?`

	var (
		record    store.Idempotency
		createdAt string
	)

	row := s.db.QueryRowContext(ctx, query, key)

	err := row.Scan(&record.Key, &record.RequestHash, &record.ProcessID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Idempotency{}, core.ErrNotFound
	}

	if err != nil {
		return store.Idempotency{}, fmt.Errorf("reading idempotency key: %w", err)
	}

	if record.CreatedAt, err = parseTime(createdAt); err != nil {
		return store.Idempotency{}, err
	}

	return record, nil
}

// claimGrace is how long a key is left alone before it may be read as an
// orphan.
//
// A claim is written just *before* the execution it points at, so between the
// two writes it legitimately points at nothing. A retention pass landing in
// that gap would delete a claim that is about to become valid, and the client
// retry it was taken for would then start the work a second time.
const claimGrace = time.Minute

// purgeIdempotency removes the keys that can no longer replay anything: those
// past the retention window, and those whose execution has already been purged
// by the row limit.
//
// A key that outlives its execution is worse than one that is simply gone: the
// replay looks it up, finds nothing, and the client is told its own key is
// unknown — for a request it may safely repeat. The caller holds writeMu.
func (s *Store) purgeIdempotency(ctx context.Context, before time.Time) error {
	const query = `DELETE FROM idempotency_keys
		WHERE created_at < ?
		   OR (created_at < ? AND process_id NOT IN (SELECT id FROM processes))`

	settled := time.Now().UTC().Add(-claimGrace)

	if _, err := s.db.ExecContext(ctx, query, formatTime(before), formatTime(settled)); err != nil {
		return fmt.Errorf("purging idempotency keys: %w", err)
	}

	return nil
}
