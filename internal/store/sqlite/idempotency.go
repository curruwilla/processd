package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

// SaveIdempotency records the execution produced for an idempotency key.
func (s *Store) SaveIdempotency(ctx context.Context, record store.Idempotency) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	const query = `INSERT INTO idempotency_keys (key, request_hash, process_id, created_at)
		VALUES (?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		record.Key, record.RequestHash, record.ProcessID, formatTime(record.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("saving idempotency key: %w", err)
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

// PurgeIdempotency removes keys recorded before the given instant.
func (s *Store) PurgeIdempotency(ctx context.Context, before string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, before); err != nil {
		return fmt.Errorf("purging idempotency keys: %w", err)
	}

	return nil
}
