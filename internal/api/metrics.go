package api

import (
	"net/http"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/metrics"
)

// metrics renders the Prometheus exposition of the node (docs/SPEC.md §18).
//
// Everything a scrape needs is gathered here, once: the state counters from the
// store, the live per-worker view from the supervisor, and the accumulated
// counters and histograms from the registry the supervisor feeds.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.CountByState(r.Context())
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	queued, err := s.store.CountPendingByWorker(r.Context())
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	used, limit := s.scheduler.Slots().Usage()

	var out metrics.Writer

	out.Gauge("processd_daemon_up", "1 while the daemon is serving", 1)
	out.Gauge("processd_slots_used", "concurrency slots currently held", float64(used))
	out.Gauge("processd_slots_max", "concurrency slots available on this node", float64(limit))
	out.Gauge("processd_workers", "worker definitions loaded", float64(s.scheduler.Registry().Len()))
	out.Gauge("processd_running_attempts", "attempts under supervision", float64(s.supervisor.Running()))
	out.Gauge("processd_queue_depth", "executions waiting for a slot",
		float64(counts[core.StateQueued]+counts[core.StateRetrying]))

	out.GaugeVec("processd_processes", "executions per state", stateSamples(counts))
	out.GaugeVec("processd_processes_queued", "executions waiting for a slot, per worker", workerSamples(queued))

	s.writeRunning(&out)
	s.registry.Write(&out)

	w.Header().Set("Content-Type", metrics.ContentType)
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte(out.String())); err != nil {
		s.log.Error("writing metrics", "error", err)
	}
}

// writeRunning renders the live view of the supervised attempts: how many run
// per worker, and what they are consuming right now.
//
// The resource sample is per worker, never per execution: a label carrying an
// execution id would add one time series per process ever run, which is how a
// Prometheus server is killed by its own exporter.
func (s *Server) writeRunning(out *metrics.Writer) {
	running := map[string]int{}
	cpu := map[string]float64{}
	rss := map[string]float64{}

	for _, sample := range s.supervisor.Snapshot() {
		running[sample.Worker]++

		usage, ok := s.supervisor.Usage(sample.ProcessID)
		if !ok {
			// The attempt ended between the snapshot and the sample.
			continue
		}

		cpu[sample.Worker] += usage.CPUSeconds
		rss[sample.Worker] += float64(usage.RSSBytes)
	}

	out.GaugeVec("processd_processes_running", "attempts under supervision, per worker", workerSamples(running))
	out.GaugeVec("processd_running_cpu_seconds", "CPU seconds consumed by the running attempts, per worker",
		floatSamples(cpu))
	out.GaugeVec("processd_running_rss_bytes", "resident memory held by the running attempts, per worker",
		floatSamples(rss))
}

func stateSamples(counts map[core.State]int) []metrics.Sample {
	samples := make([]metrics.Sample, 0, len(counts))
	for state, count := range counts {
		samples = append(samples, metrics.Sample{
			Labels: []metrics.Label{{Name: "state", Value: string(state)}},
			Value:  float64(count),
		})
	}

	return samples
}

func workerSamples(counts map[string]int) []metrics.Sample {
	byWorker := make(map[string]float64, len(counts))
	for worker, count := range counts {
		byWorker[worker] = float64(count)
	}

	return floatSamples(byWorker)
}

func floatSamples(values map[string]float64) []metrics.Sample {
	samples := make([]metrics.Sample, 0, len(values))
	for worker, value := range values {
		samples = append(samples, metrics.Sample{
			Labels: []metrics.Label{{Name: "worker", Value: worker}},
			Value:  value,
		})
	}

	return samples
}
