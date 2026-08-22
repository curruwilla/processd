package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/curruwilla/processd/internal/store"
)

// LoadSchedules returns the persisted state of every schedule, keyed by worker.
//
// It is read once at startup and after a reload: the firing loop keeps the
// answer in memory, because a schedule that has to query the database to decide
// whether to fire has made the database a dependency of the clock.
func (s *Store) LoadSchedules(ctx context.Context) (map[string]store.ScheduleState, error) {
	const query = `SELECT worker, last_fired_at, last_missed_at, missed_runs FROM schedules`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("reading schedules: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()

	states := map[string]store.ScheduleState{}

	for rows.Next() {
		var (
			state      store.ScheduleState
			lastFired  sql.NullString
			lastMissed sql.NullString
		)

		if err := rows.Scan(&state.Worker, &lastFired, &lastMissed, &state.MissedRuns); err != nil {
			return nil, fmt.Errorf("scanning schedule: %w", err)
		}

		if state.LastFiredAt, err = parseTimePtr(lastFired); err != nil {
			return nil, err
		}

		if state.LastMissedAt, err = parseTimePtr(lastMissed); err != nil {
			return nil, err
		}

		states[state.Worker] = state
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading schedules: %w", err)
	}

	return states, nil
}

// SaveSchedule writes the state of one schedule, replacing what was there.
func (s *Store) SaveSchedule(ctx context.Context, state store.ScheduleState) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	const query = `INSERT INTO schedules (worker, last_fired_at, last_missed_at, missed_runs)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (worker) DO UPDATE SET
			last_fired_at  = excluded.last_fired_at,
			last_missed_at = excluded.last_missed_at,
			missed_runs    = excluded.missed_runs`

	_, err := s.db.ExecContext(ctx, query,
		state.Worker,
		formatTimePtr(state.LastFiredAt),
		formatTimePtr(state.LastMissedAt),
		state.MissedRuns,
	)
	if err != nil {
		return fmt.Errorf("saving schedule for worker %q: %w", state.Worker, err)
	}

	return nil
}
