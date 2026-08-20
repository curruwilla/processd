package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/curruwilla/processd/internal/core"
)

// metricsContentType is the Prometheus text exposition format. The daemon emits
// it by hand: a handful of gauges does not justify a client library.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.CountByState(r.Context())
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	used, limit := s.scheduler.Slots().Usage()

	var out strings.Builder

	writeGauge(&out, "processd_daemon_up", "1 while the daemon is serving", 1)
	writeGauge(&out, "processd_slots_used", "concurrency slots currently held", float64(used))
	writeGauge(&out, "processd_slots_max", "concurrency slots available on this node", float64(limit))
	writeGauge(&out, "processd_workers", "worker definitions loaded", float64(s.scheduler.Registry().Len()))
	writeGauge(&out, "processd_running_attempts", "attempts under supervision", float64(s.supervisor.Running()))
	writeGauge(&out, "processd_queue_depth", "executions waiting for a slot",
		float64(counts[core.StateQueued]+counts[core.StateRetrying]))

	fmt.Fprintln(&out, "# HELP processd_processes executions per state")
	fmt.Fprintln(&out, "# TYPE processd_processes gauge")

	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, string(state))
	}

	sort.Strings(states)

	for _, state := range states {
		fmt.Fprintf(&out, "processd_processes{state=%q} %d\n", state, counts[core.State(state)])
	}

	w.Header().Set("Content-Type", metricsContentType)
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte(out.String())); err != nil {
		s.log.Error("writing metrics", "error", err)
	}
}

func writeGauge(out *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(out, "# HELP %s %s\n", name, help)
	fmt.Fprintf(out, "# TYPE %s gauge\n", name)
	fmt.Fprintf(out, "%s %g\n", name, value)
}
