package metrics

import (
	"slices"
	"strconv"
	"sync"
	"time"
)

// workerLabel names the dimension every per-worker family carries.
const workerLabel = "worker"

// durationBuckets are the upper bounds of the execution duration histogram, in
// seconds. They span the range this daemon actually sees: sub-second scripts at
// one end, hour-long batch jobs at the other.
var durationBuckets = []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 900, 3600}

// outcomeKey identifies one worker/state pair of the outcome counter.
type outcomeKey struct {
	worker string
	state  string
}

// Registry accumulates what only the supervisor can see: how many attempts
// started, how they ended and how long they took.
//
// The values are in memory and reset with the daemon, which is what Prometheus
// expects of a process-local counter — a restart is visible as a counter reset,
// not as a gap.
type Registry struct {
	mu        sync.Mutex
	attempts  map[string]uint64
	outcomes  map[outcomeKey]uint64
	durations map[string]*histogram
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		attempts:  map[string]uint64{},
		outcomes:  map[outcomeKey]uint64{},
		durations: map[string]*histogram{},
	}
}

// AttemptStarted records that one attempt of a worker began running.
func (r *Registry) AttemptStarted(worker string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.attempts[worker]++
}

// AttemptFinished records the outcome of an attempt and how long it ran. A
// non-positive duration is not observed: an attempt that never started carries
// no timing to report.
func (r *Registry) AttemptFinished(worker, state string, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.outcomes[outcomeKey{worker: worker, state: state}]++

	if elapsed <= 0 {
		return
	}

	observed, ok := r.durations[worker]
	if !ok {
		observed = newHistogram()
		r.durations[worker] = observed
	}

	observed.observe(elapsed.Seconds())
}

// Write renders every family the registry owns.
func (r *Registry) Write(w *Writer) {
	r.mu.Lock()

	attempts := make([]Sample, 0, len(r.attempts))
	for worker, count := range r.attempts {
		attempts = append(attempts, Sample{
			Labels: []Label{{Name: workerLabel, Value: worker}},
			Value:  float64(count),
		})
	}

	outcomes := make([]Sample, 0, len(r.outcomes))
	for key, count := range r.outcomes {
		outcomes = append(outcomes, Sample{
			Labels: []Label{{Name: workerLabel, Value: key.worker}, {Name: "status", Value: key.state}},
			Value:  float64(count),
		})
	}

	durations := make(map[string]histogram, len(r.durations))
	for worker, observed := range r.durations {
		durations[worker] = observed.snapshot()
	}

	r.mu.Unlock()

	w.CounterVec("processd_process_attempts_total", "attempts started per worker", attempts)
	w.CounterVec("processd_processes_total", "executions finished per worker and terminal state", outcomes)
	writeHistograms(w, "processd_process_duration_seconds", "attempt duration in seconds", durations)
}

// histogram is a cumulative histogram over durationBuckets.
type histogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

func newHistogram() *histogram {
	return &histogram{counts: make([]uint64, len(durationBuckets))}
}

// observe records one value. Values above the last bucket only reach the
// implicit +Inf bucket, which is written from the total.
func (h *histogram) observe(value float64) {
	for i, bound := range durationBuckets {
		if value <= bound {
			h.counts[i]++
		}
	}

	h.sum += value
	h.total++
}

func (h *histogram) snapshot() histogram {
	counts := make([]uint64, len(h.counts))
	copy(counts, h.counts)

	return histogram{counts: counts, sum: h.sum, total: h.total}
}

// writeHistograms renders one histogram family, one series per worker.
func writeHistograms(w *Writer, name, help string, byWorker map[string]histogram) {
	w.header(name, help, "histogram")

	workers := make([]string, 0, len(byWorker))
	for worker := range byWorker {
		workers = append(workers, worker)
	}

	slices.Sort(workers)

	for _, worker := range workers {
		observed := byWorker[worker]
		label := Label{Name: workerLabel, Value: worker}

		for i, bound := range durationBuckets {
			w.sample(name+"_bucket", []Label{label, {Name: "le", Value: formatBound(bound)}}, float64(observed.counts[i]))
		}

		w.sample(name+"_bucket", []Label{label, {Name: "le", Value: "+Inf"}}, float64(observed.total))
		w.sample(name+"_sum", []Label{label}, observed.sum)
		w.sample(name+"_count", []Label{label}, float64(observed.total))
	}
}

// formatBound renders a bucket bound the way Prometheus writes it, so that a
// scrape sees "0.1" and "3600", never "1e+02".
func formatBound(bound float64) string {
	return strconv.FormatFloat(bound, 'f', -1, 64)
}
