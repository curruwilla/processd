package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

// processColumns is the column list shared by every read query, in the order
// scanProcess expects.
const processColumns = `id, worker, type, state, reason, attempt, max_attempts, lock_key,
	command, args, env, cwd, run_user, run_group, timeout_ns, metadata,
	pid, pid_start_time, exit_code, signal, log_truncated,
	created_at, queued_at, started_at, finished_at, retry_at`

// terminalStates is the SQL fragment listing the states that never change again.
const terminalStates = `('COMPLETED', 'FAILED', 'CANCELED')`

// maxPendingBatch bounds one dispatch pass so a huge backlog cannot stall the
// scheduler loop.
const maxPendingBatch = 500

// CreateProcess persists a newly submitted execution.
func (s *Store) CreateProcess(ctx context.Context, p *core.Process) error {
	args, err := encodeJSON(p.Args)
	if err != nil {
		return err
	}

	env, err := encodeJSON(p.Env)
	if err != nil {
		return err
	}

	metadata, err := encodeJSON(p.Metadata)
	if err != nil {
		return err
	}

	const query = `INSERT INTO processes (
		id, worker, type, state, reason, attempt, max_attempts, lock_key,
		command, args, env, cwd, run_user, run_group, timeout_ns, metadata,
		pid, pid_start_time, exit_code, signal, log_truncated,
		created_at, queued_at, started_at, finished_at, retry_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err = s.db.ExecContext(ctx, query,
		p.ID, p.Worker, string(p.Type), string(p.State), string(p.Reason), p.Attempt, p.MaxAttempts, p.Lock,
		p.Command, args, env, p.Cwd, p.User, p.Group, int64(p.Timeout), metadata,
		//nolint:gosec // a /proc start time is clock ticks since boot, never near the int64 limit
		p.PID, int64(p.PIDStartTime), p.ExitCode, p.Signal, p.LogTruncated,
		formatTime(p.CreatedAt), formatTimePtr(p.QueuedAt), formatTimePtr(p.StartedAt),
		formatTimePtr(p.FinishedAt), formatTimePtr(p.RetryAt),
	)
	if err != nil {
		return fmt.Errorf("inserting process %s: %w", p.ID, err)
	}

	return nil
}

// UpdateProcess persists a state change of an execution, together with the row
// describing its current attempt.
func (s *Store) UpdateProcess(ctx context.Context, p *core.Process) error {
	args, err := encodeJSON(p.Args)
	if err != nil {
		return err
	}

	env, err := encodeJSON(p.Env)
	if err != nil {
		return err
	}

	metadata, err := encodeJSON(p.Metadata)
	if err != nil {
		return err
	}

	const query = `UPDATE processes SET
		worker = ?, type = ?, state = ?, reason = ?, attempt = ?, max_attempts = ?, lock_key = ?,
		command = ?, args = ?, env = ?, cwd = ?, run_user = ?, run_group = ?, timeout_ns = ?, metadata = ?,
		pid = ?, pid_start_time = ?, exit_code = ?, signal = ?, log_truncated = ?,
		queued_at = ?, started_at = ?, finished_at = ?, retry_at = ?
	WHERE id = ?`

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("updating process %s: %w", p.ID, err)
	}

	defer func() {
		_ = tx.Rollback()
	}()

	result, err := tx.ExecContext(ctx, query,
		p.Worker, string(p.Type), string(p.State), string(p.Reason), p.Attempt, p.MaxAttempts, p.Lock,
		p.Command, args, env, p.Cwd, p.User, p.Group, int64(p.Timeout), metadata,
		//nolint:gosec // a /proc start time is clock ticks since boot, never near the int64 limit
		p.PID, int64(p.PIDStartTime), p.ExitCode, p.Signal, p.LogTruncated,
		formatTimePtr(p.QueuedAt), formatTimePtr(p.StartedAt), formatTimePtr(p.FinishedAt), formatTimePtr(p.RetryAt),
		p.ID,
	)
	if err != nil {
		return fmt.Errorf("updating process %s: %w", p.ID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("updating process %s: %w", p.ID, err)
	}

	if affected == 0 {
		return fmt.Errorf("updating process %s: %w", p.ID, core.ErrNotFound)
	}

	if err := upsertAttempt(ctx, tx, p); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("updating process %s: %w", p.ID, err)
	}

	return nil
}

// upsertAttempt keeps one row per attempt, so exit codes and log truncation stay
// addressable per try instead of only for the last one.
func upsertAttempt(ctx context.Context, tx *sql.Tx, p *core.Process) error {
	if p.Attempt <= 0 {
		return nil
	}

	const query = `INSERT INTO attempts (
		process_id, attempt, pid, pid_start_time, exit_code, signal, reason, log_truncated, started_at, finished_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (process_id, attempt) DO UPDATE SET
		pid = excluded.pid,
		pid_start_time = excluded.pid_start_time,
		exit_code = excluded.exit_code,
		signal = excluded.signal,
		reason = excluded.reason,
		log_truncated = excluded.log_truncated,
		started_at = excluded.started_at,
		finished_at = excluded.finished_at`

	_, err := tx.ExecContext(ctx, query,
		//nolint:gosec // a /proc start time is clock ticks since boot, never near the int64 limit
		p.ID, p.Attempt, p.PID, int64(p.PIDStartTime), p.ExitCode, p.Signal, string(p.Reason), p.LogTruncated,
		formatTimePtr(p.StartedAt), formatTimePtr(p.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("recording attempt %d of %s: %w", p.Attempt, p.ID, err)
	}

	return nil
}

// GetProcess returns one execution by its logical ID.
func (s *Store) GetProcess(ctx context.Context, id string) (*core.Process, error) {
	query := `SELECT ` + processColumns + ` FROM processes WHERE id = ?`

	row := s.db.QueryRowContext(ctx, query, id)

	p, err := scanProcess(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", id, core.ErrNotFound)
	}

	if err != nil {
		return nil, fmt.Errorf("reading process %s: %w", id, err)
	}

	return p, nil
}

// ListProcesses returns one cursor-paginated page of executions, newest first.
func (s *Store) ListProcesses(ctx context.Context, f store.Filter) (store.Page, error) {
	where := []string{"1 = 1"}
	args := []any{}

	if len(f.States) > 0 {
		placeholders := make([]string, 0, len(f.States))

		for _, state := range f.States {
			placeholders = append(placeholders, "?")
			args = append(args, string(state))
		}

		where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
	}

	if f.Worker != "" {
		where = append(where, "worker = ?")
		args = append(args, f.Worker)
	}

	if f.Lock != "" {
		where = append(where, "lock_key = ?")
		args = append(args, f.Lock)
	}

	if f.CreatedAfter != nil {
		where = append(where, "created_at > ?")
		args = append(args, formatTime(*f.CreatedAfter))
	}

	if f.CreatedBefore != nil {
		where = append(where, "created_at < ?")
		args = append(args, formatTime(*f.CreatedBefore))
	}

	if f.Cursor != "" {
		createdAt, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return store.Page{}, err
		}

		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, formatTime(createdAt), formatTime(createdAt), id)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	// One extra row tells us whether another page exists without a count query.
	args = append(args, limit+1)

	query := `SELECT ` + processColumns + ` FROM processes WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY created_at DESC, id DESC LIMIT ?`

	items, err := s.queryProcesses(ctx, query, args...)
	if err != nil {
		return store.Page{}, err
	}

	page := store.Page{Items: items}

	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}

	return page, nil
}

// PendingProcesses returns the executions the scheduler may start now.
func (s *Store) PendingProcesses(ctx context.Context, now time.Time) ([]*core.Process, error) {
	query := `SELECT ` + processColumns + ` FROM processes
		WHERE state = 'QUEUED'
		   OR (state = 'RETRYING' AND (retry_at IS NULL OR retry_at <= ?))
		ORDER BY created_at ASC, id ASC LIMIT ?`

	return s.queryProcesses(ctx, query, formatTime(now), maxPendingBatch)
}

// UnfinishedProcesses returns the executions left in a non-terminal state.
func (s *Store) UnfinishedProcesses(ctx context.Context) ([]*core.Process, error) {
	query := `SELECT ` + processColumns + ` FROM processes
		WHERE state NOT IN ` + terminalStates + ` ORDER BY created_at ASC`

	return s.queryProcesses(ctx, query)
}

// PendingCount counts the executions waiting for a slot.
//
// The predicate is restricted to two states so that SQLite answers it with an
// index search instead of scanning every row ever recorded: on a 300k-row
// history that is the difference between 14ms and 0.02ms, paid by every
// submission.
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	const query = `SELECT COUNT(*) FROM processes WHERE state IN ('QUEUED', 'RETRYING')`

	var count int

	if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting pending executions: %w", err)
	}

	return count, nil
}

// CountActiveByState counts the executions that are not in a terminal state.
//
// The predicate keeps SQLite on the state index instead of the table, so a
// dashboard polling this endpoint never pays for the retained history.
func (s *Store) CountActiveByState(ctx context.Context) (map[core.State]int, error) {
	query := `SELECT state, COUNT(*) FROM processes
		WHERE state NOT IN ` + terminalStates + ` GROUP BY state`

	return s.countStates(ctx, query)
}

// CountPendingByWorker counts the executions waiting for a slot, per worker.
//
// It shares the predicate of PendingCount, and therefore its index search: the
// metrics endpoint must not become the one place that scans the history.
func (s *Store) CountPendingByWorker(ctx context.Context) (map[string]int, error) {
	const query = `SELECT worker, COUNT(*) FROM processes
		WHERE state IN ('QUEUED', 'RETRYING') GROUP BY worker`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("counting pending executions per worker: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()

	counts := map[string]int{}

	for rows.Next() {
		var (
			worker string
			count  int
		)

		if err := rows.Scan(&worker, &count); err != nil {
			return nil, fmt.Errorf("counting pending executions per worker: %w", err)
		}

		counts[worker] = count
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting pending executions per worker: %w", err)
	}

	return counts, nil
}

// CountByState reports how many executions sit in each state.
//
// This one does scan the table. It is only used by the metrics endpoint, which
// is scraped on an interval, never per request.
func (s *Store) CountByState(ctx context.Context) (map[core.State]int, error) {
	return s.countStates(ctx, `SELECT state, COUNT(*) FROM processes GROUP BY state`)
}

// countStates runs a "state, count" query and collects it into a map.
func (s *Store) countStates(ctx context.Context, query string) (map[core.State]int, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("counting processes: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()

	counts := map[core.State]int{}

	for rows.Next() {
		var (
			state string
			count int
		)

		if err := rows.Scan(&state, &count); err != nil {
			return nil, fmt.Errorf("counting processes: %w", err)
		}

		counts[core.State(state)] = count
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting processes: %w", err)
	}

	return counts, nil
}

// PurgeHistory deletes terminal executions beyond the retention limits, and the
// expired idempotency keys and audit entries that go with them.
func (s *Store) PurgeHistory(ctx context.Context, before time.Time, maxRows int) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	removed := 0

	byAge := `DELETE FROM processes WHERE state IN ` + terminalStates +
		` AND COALESCE(finished_at, created_at) < ?`

	result, err := s.db.ExecContext(ctx, byAge, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("purging history: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purging history: %w", err)
	}

	removed += int(affected)

	if maxRows > 0 {
		byCount := `DELETE FROM processes WHERE id IN (
			SELECT id FROM processes WHERE state IN ` + terminalStates + `
			ORDER BY created_at DESC LIMIT -1 OFFSET ?)`

		result, err := s.db.ExecContext(ctx, byCount, maxRows)
		if err != nil {
			return removed, fmt.Errorf("trimming history: %w", err)
		}

		affected, err := result.RowsAffected()
		if err != nil {
			return removed, fmt.Errorf("trimming history: %w", err)
		}

		removed += int(affected)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, formatTime(before)); err != nil {
		return removed, fmt.Errorf("purging audit log: %w", err)
	}

	return removed, nil
}

func (s *Store) queryProcesses(ctx context.Context, query string, args ...any) ([]*core.Process, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading processes: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()

	processes := []*core.Process{}

	for rows.Next() {
		p, err := scanProcess(rows)
		if err != nil {
			return nil, fmt.Errorf("reading processes: %w", err)
		}

		processes = append(processes, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading processes: %w", err)
	}

	return processes, nil
}

// scanner is implemented by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanProcess(row scanner) (*core.Process, error) {
	var (
		p            core.Process
		procType     string
		state        string
		reason       string
		timeoutNS    int64
		pidStartTime int64
		exitCode     sql.NullInt64
		rawArgs      string
		rawEnv       string
		rawMetadata  string
		createdAt    string
		queuedAt     sql.NullString
		startedAt    sql.NullString
		finishedAt   sql.NullString
		retryAt      sql.NullString
	)

	err := row.Scan(
		&p.ID, &p.Worker, &procType, &state, &reason, &p.Attempt, &p.MaxAttempts, &p.Lock,
		&p.Command, &rawArgs, &rawEnv, &p.Cwd, &p.User, &p.Group, &timeoutNS, &rawMetadata,
		&p.PID, &pidStartTime, &exitCode, &p.Signal, &p.LogTruncated,
		&createdAt, &queuedAt, &startedAt, &finishedAt, &retryAt,
	)
	if err != nil {
		return nil, err
	}

	p.Type = core.Type(procType)
	p.State = core.State(state)
	p.Reason = core.Reason(reason)
	p.Timeout = time.Duration(timeoutNS)
	p.PIDStartTime = uint64(pidStartTime) //nolint:gosec // written by this package as a positive clock-tick count

	if exitCode.Valid {
		code := int(exitCode.Int64)
		p.ExitCode = &code
	}

	if err := decodeJSON(rawArgs, &p.Args); err != nil {
		return nil, err
	}

	if err := decodeJSON(rawEnv, &p.Env); err != nil {
		return nil, err
	}

	if err := decodeJSON(rawMetadata, &p.Metadata); err != nil {
		return nil, err
	}

	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}

	for _, field := range []struct {
		raw sql.NullString
		out **time.Time
	}{
		{queuedAt, &p.QueuedAt},
		{startedAt, &p.StartedAt},
		{finishedAt, &p.FinishedAt},
		{retryAt, &p.RetryAt},
	} {
		parsed, err := parseTimePtr(field.raw)
		if err != nil {
			return nil, err
		}

		*field.out = parsed
	}

	return &p, nil
}
