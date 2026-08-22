// Package schedule fires workers on their own cron expression, so that a
// scheduled job is owned by the daemon that runs it rather than by a crontab
// entry holding an API token (docs/SPEC.md §22.1).
package schedule

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/cron"
	"github.com/curruwilla/processd/internal/store"
)

const (
	// maxSleep bounds how long the loop waits between passes even when the next
	// occurrence is days away.
	//
	// A timer armed for tomorrow sleeps through a suspend, an NTP step or a
	// zone-database update and wakes convinced nothing happened. Re-deriving the
	// plan every minute costs nothing and makes the clock something the loop
	// reads rather than something it assumes.
	maxSleep = time.Minute

	// maxMissed bounds how many missed occurrences are enumerated after
	// downtime. A minutely schedule and a week of downtime describe ten thousand
	// occurrences, and materialising them to count them turns a recovery path
	// into a second outage. Past the cap the count is reported as a floor.
	maxMissed = 1000

	// occurrenceLayout renders an occurrence as the wall clock shows it.
	//
	// Deduplication happens on this string, not on the instant: when the clock
	// falls back, one local time maps to two instants, and a schedule that reads
	// "03:00 daily" must mean one firing that day.
	occurrenceLayout = "2006-01-02T15:04"

	// triggerKey and triggerSchedule mark an execution the daemon started by
	// itself, so that the history distinguishes it from a client request.
	triggerKey      = "processd.trigger"
	triggerSchedule = "schedule"

	// occurrenceKey records which occurrence the execution was created for. It
	// is not the creation time: jitter and dispatch delay move that.
	occurrenceKey = "processd.occurrence"
)

// Submitter admits an execution. It is the scheduler, narrowed to what firing
// needs.
type Submitter interface {
	Submit(ctx context.Context, p *core.Process) error
}

// Status is what the API reports about one worker's schedule.
type Status struct {
	Cron         string
	Timezone     string
	NextRun      *time.Time
	LastFiredAt  *time.Time
	LastMissedAt *time.Time
	MissedRuns   int
}

// entry is the in-memory plan for one scheduled worker. It is rebuilt from the
// registry and the store on every reset, never patched in place.
type entry struct {
	worker   string
	schedule *cron.Schedule
	jitter   time.Duration

	// occurrence is the scheduled instant the next firing belongs to, and
	// fireAt is when it will actually be submitted. They differ by jitter.
	occurrence time.Time
	fireAt     time.Time

	// planned is false for a schedule with no reachable occurrence, such as
	// 30 February. It stays loaded and reports nothing.
	planned bool
}

// Runner owns the firing loop. One goroutine, driven by a timer.
type Runner struct {
	workers func() *config.Registry
	submit  Submitter
	store   store.Store
	log     *slog.Logger

	// reloaded asks the loop to rebuild its plan after workers.d changed.
	reloaded chan struct{}

	mu      sync.Mutex
	entries map[string]*entry
	states  map[string]store.ScheduleState
}

// New wires a runner. Dependencies are passed explicitly, as everywhere else in
// the object graph.
func New(workers func() *config.Registry, submit Submitter, st store.Store, log *slog.Logger) *Runner {
	return &Runner{
		workers:  workers,
		submit:   submit,
		store:    st,
		log:      log,
		reloaded: make(chan struct{}, 1),
		entries:  map[string]*entry{},
		states:   map[string]store.ScheduleState{},
	}
}

// Reload asks the loop to rebuild its plan. It never blocks: a reload that
// arrives while one is pending is the same reload.
func (r *Runner) Reload() {
	select {
	case r.reloaded <- struct{}{}:
	default:
	}
}

// Run drives the firing loop until the context is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.reset(ctx, time.Now())

	timer := time.NewTimer(r.sleep(time.Now()))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.reloaded:
			r.reset(ctx, time.Now())
		case <-timer.C:
			r.fireDue(ctx, time.Now())
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		timer.Reset(r.sleep(time.Now()))
	}
}

// Status returns what is known about a worker's schedule, or false when the
// worker has none.
func (r *Runner) Status(worker string) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	planned, ok := r.entries[worker]
	if !ok {
		return Status{}, false
	}

	status := Status{
		Cron:     planned.schedule.Spec(),
		Timezone: planned.schedule.Location().String(),
	}

	if planned.planned {
		next := planned.occurrence
		status.NextRun = &next
	}

	if state, ok := r.states[worker]; ok {
		status.LastFiredAt = state.LastFiredAt
		status.LastMissedAt = state.LastMissedAt
		status.MissedRuns = state.MissedRuns
	}

	return status, true
}

// sleep reports how long the loop may wait before its next pass.
func (r *Runner) sleep(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	wait := maxSleep

	for _, planned := range r.entries {
		if !planned.planned {
			continue
		}

		if until := planned.fireAt.Sub(now); until < wait {
			wait = until
		}
	}

	if wait < 0 {
		return 0
	}

	return wait
}

// reset rebuilds the plan from the registry and the persisted state.
//
// It runs at startup and after every reload, and it is the only place that
// accounts for occurrences the daemon was not running for.
func (r *Runner) reset(ctx context.Context, now time.Time) {
	stored, err := r.store.LoadSchedules(ctx)
	if err != nil {
		// A schedule with no history fires from now on rather than not at all:
		// losing the record of the last firing must not stop the next one.
		r.log.Error("loading schedules", slog.Any("error", err))

		stored = map[string]store.ScheduleState{}
	}

	entries := map[string]*entry{}
	states := map[string]store.ScheduleState{}

	for _, worker := range r.workers().All() {
		if !worker.Schedule.IsSet() || worker.Schedule.Compiled() == nil {
			continue
		}

		planned := &entry{
			worker:   worker.Name,
			schedule: worker.Schedule.Compiled(),
			jitter:   worker.Schedule.Jitter.Duration(),
		}

		state, ok := stored[worker.Name]
		if !ok {
			state = store.ScheduleState{Worker: worker.Name}
		}

		r.accountForDowntime(ctx, planned, worker.Schedule.CatchUp, &state, now)

		entries[worker.Name] = planned
		states[worker.Name] = state
	}

	r.mu.Lock()
	r.entries = entries
	r.states = states
	r.mu.Unlock()

	if len(entries) > 0 {
		r.log.Info("schedules loaded", slog.Int("count", len(entries)))
	}
}

// accountForDowntime plans the next firing, reporting whatever passed while the
// daemon was not running.
//
// A missed occurrence is recorded whether or not it is caught up: an operator
// has to be able to see that the job did not run, and silence is the failure
// mode an external crontab already has.
func (r *Runner) accountForDowntime(
	ctx context.Context,
	planned *entry,
	catchUp bool,
	state *store.ScheduleState,
	now time.Time,
) {
	if state.LastFiredAt == nil {
		// Nothing ran before, so nothing was missed: a schedule added today did
		// not fail to run last week.
		r.plan(planned, now)
		return
	}

	missed := planned.schedule.Between(*state.LastFiredAt, now, maxMissed)
	if len(missed) == 0 {
		r.plan(planned, now)
		return
	}

	last := missed[len(missed)-1]

	if catchUp {
		// One firing, not one per missed occurrence: catching up an entire
		// weekend of a minutely schedule is an incident, not a recovery.
		planned.occurrence = last
		planned.fireAt = now
		planned.planned = true

		r.log.Warn("catching up a missed schedule",
			slog.String("worker", planned.worker),
			slog.Int("missed", len(missed)),
			slog.Time("occurrence", last),
		)

		return
	}

	state.MissedRuns += len(missed)
	state.LastMissedAt = &last

	r.log.Warn("schedule missed occurrences while the daemon was down",
		slog.String("worker", planned.worker),
		slog.Int("missed", len(missed)),
		slog.Bool("truncated", len(missed) == maxMissed),
		slog.Time("last", last),
	)

	if err := r.store.SaveSchedule(ctx, *state); err != nil {
		r.log.Error("recording missed schedule",
			slog.String("worker", planned.worker), slog.Any("error", err))
	}

	r.plan(planned, now)
}

// plan points an entry at the first occurrence after from.
func (r *Runner) plan(planned *entry, from time.Time) {
	occurrence, ok := planned.schedule.Next(from)
	if !ok {
		planned.planned = false

		r.log.Warn("schedule has no reachable occurrence",
			slog.String("worker", planned.worker),
			slog.String("cron", planned.schedule.Spec()),
		)

		return
	}

	planned.occurrence = occurrence
	planned.fireAt = occurrence.Add(jitterOf(planned.jitter))
	planned.planned = true
}

// jitterOf spreads a firing across the configured window.
func jitterOf(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}

	//nolint:gosec // spreading firings across a fleet needs no cryptographic randomness
	return time.Duration(rand.Int64N(int64(window) + 1))
}

// fireDue submits every entry whose firing time has arrived.
func (r *Runner) fireDue(ctx context.Context, now time.Time) {
	r.mu.Lock()

	due := make([]*entry, 0, len(r.entries))

	for _, planned := range r.entries {
		if planned.planned && !planned.fireAt.After(now) {
			due = append(due, planned)
		}
	}

	r.mu.Unlock()

	for _, planned := range due {
		r.fire(ctx, planned, now)
		r.advance(ctx, planned, now)
	}
}

// fire submits one occurrence.
//
// Every outcome advances the schedule, including a refusal: a queue that is
// full or a lock that is held is an answer about this occurrence, not a reason
// to hold the grid still and try again a minute later.
func (r *Runner) fire(ctx context.Context, planned *entry, now time.Time) {
	worker, err := r.workers().Get(planned.worker)
	if err != nil {
		// The worker went away between the reset and the tick. The reload that
		// removed it will drop the entry.
		return
	}

	if !worker.IsEnabled() {
		r.log.Info("skipping schedule of a disabled worker", slog.String("worker", worker.Name))
		return
	}

	if r.alreadyFired(planned) {
		// The clock fell back and handed us the same local time twice.
		r.log.Info("skipping a repeated local occurrence",
			slog.String("worker", worker.Name),
			slog.Time("occurrence", planned.occurrence),
		)

		return
	}

	process, err := worker.Instantiate(nil)
	if err != nil {
		r.log.Error("building a scheduled execution",
			slog.String("worker", worker.Name), slog.Any("error", err))

		return
	}

	// Overlap is not a new concept: a schedule that has no lock of its own gets
	// one keyed to the worker, and lock_conflict decides whether the next
	// firing waits or is refused (docs/SPEC.md §15).
	if process.Lock == "" {
		process.Lock = "schedule:" + worker.Name
	}

	process.Metadata = map[string]string{
		triggerKey:    triggerSchedule,
		occurrenceKey: planned.occurrence.Format(time.RFC3339),
	}

	if err := r.submit.Submit(ctx, process); err != nil {
		// Submit records the refusal on the execution itself, so the history
		// already carries what happened; the log says which schedule caused it.
		r.log.Warn("scheduled execution was not admitted",
			slog.String("worker", worker.Name),
			slog.String("process", process.ID),
			slog.Any("error", err),
		)
	} else {
		r.log.Info("schedule fired",
			slog.String("worker", worker.Name),
			slog.String("process", process.ID),
			slog.Time("occurrence", planned.occurrence),
			slog.Duration("late", now.Sub(planned.occurrence)),
		)
	}

	r.recordFiring(ctx, planned)
}

// alreadyFired reports whether this local occurrence has been fired before.
func (r *Runner) alreadyFired(planned *entry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.states[planned.worker]
	if !ok || state.LastFiredAt == nil {
		return false
	}

	location := planned.schedule.Location()

	return state.LastFiredAt.In(location).Format(occurrenceLayout) ==
		planned.occurrence.In(location).Format(occurrenceLayout)
}

// recordFiring persists the occurrence the schedule last fired for.
func (r *Runner) recordFiring(ctx context.Context, planned *entry) {
	r.mu.Lock()

	state, ok := r.states[planned.worker]
	if !ok {
		state = store.ScheduleState{Worker: planned.worker}
	}

	occurrence := planned.occurrence
	state.LastFiredAt = &occurrence
	r.states[planned.worker] = state

	r.mu.Unlock()

	if err := r.store.SaveSchedule(ctx, state); err != nil {
		// Losing the record risks one repeated firing after a restart, which is
		// worth a loud log and not worth failing the execution that just ran.
		r.log.Error("recording a schedule firing",
			slog.String("worker", planned.worker), slog.Any("error", err))
	}
}

// advance moves an entry to its next occurrence.
//
// When the loop has fallen behind — a long stall, a suspended host — the
// occurrences in between are counted as missed and skipped, rather than fired
// one per pass until the backlog drains.
func (r *Runner) advance(ctx context.Context, planned *entry, now time.Time) {
	next, ok := planned.schedule.Next(planned.occurrence)
	if !ok {
		planned.planned = false
		return
	}

	if next.After(now) {
		planned.occurrence = next
		planned.fireAt = next.Add(jitterOf(planned.jitter))
		planned.planned = true

		return
	}

	skipped := planned.schedule.Between(planned.occurrence, now, maxMissed)

	r.mu.Lock()

	state, ok := r.states[planned.worker]
	if !ok {
		state = store.ScheduleState{Worker: planned.worker}
	}

	state.MissedRuns += len(skipped)

	if len(skipped) > 0 {
		last := skipped[len(skipped)-1]
		state.LastMissedAt = &last
	}

	r.states[planned.worker] = state

	r.mu.Unlock()

	r.log.Warn("schedule fell behind and skipped occurrences",
		slog.String("worker", planned.worker),
		slog.Int("skipped", len(skipped)),
	)

	if err := r.store.SaveSchedule(ctx, state); err != nil {
		r.log.Error("recording skipped occurrences",
			slog.String("worker", planned.worker), slog.Any("error", err))
	}

	r.plan(planned, now)
}
