package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestRegistry_WriteCounters(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.AttemptStarted("invoice")
	registry.AttemptStarted("invoice")
	registry.AttemptStarted("report")
	registry.AttemptFinished("invoice", "COMPLETED", 2*time.Second)
	registry.AttemptFinished("invoice", "FAILED", 0)

	var out Writer

	registry.Write(&out)
	rendered := out.String()

	want := []string{
		`processd_process_attempts_total{worker="invoice"} 2`,
		`processd_process_attempts_total{worker="report"} 1`,
		`processd_processes_total{worker="invoice",status="COMPLETED"} 1`,
		`processd_processes_total{worker="invoice",status="FAILED"} 1`,
		`processd_process_duration_seconds_count{worker="invoice"} 1`,
		`processd_process_duration_seconds_sum{worker="invoice"} 2`,
		`processd_process_duration_seconds_bucket{worker="invoice",le="0.5"} 0`,
		`processd_process_duration_seconds_bucket{worker="invoice",le="5"} 1`,
		`processd_process_duration_seconds_bucket{worker="invoice",le="+Inf"} 1`,
	}

	for _, line := range want {
		if !strings.Contains(rendered, line+"\n") {
			t.Errorf("exposition is missing %q, got:\n%s", line, rendered)
		}
	}
}

func TestRegistry_FailedStartHasNoDuration(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.AttemptFinished("invoice", "FAILED", 0)

	var out Writer

	registry.Write(&out)

	if strings.Contains(out.String(), "processd_process_duration_seconds_count") {
		t.Errorf("an attempt that never ran must not be timed, got:\n%s", out.String())
	}
}

func TestWriter_Gauge(t *testing.T) {
	t.Parallel()

	var out Writer

	out.Gauge("processd_daemon_up", "1 while the daemon is serving", 1)

	want := "# HELP processd_daemon_up 1 while the daemon is serving\n" +
		"# TYPE processd_daemon_up gauge\n" +
		"processd_daemon_up 1\n"

	if out.String() != want {
		t.Errorf("Gauge() wrote:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestWriter_SamplesAreOrdered(t *testing.T) {
	t.Parallel()

	var out Writer

	out.GaugeVec("processd_processes_running", "attempts", []Sample{
		{Labels: []Label{{Name: "worker", Value: "zeta"}}, Value: 1},
		{Labels: []Label{{Name: "worker", Value: "alpha"}}, Value: 2},
	})

	alpha := strings.Index(out.String(), `worker="alpha"`)
	zeta := strings.Index(out.String(), `worker="zeta"`)

	if alpha < 0 || zeta < 0 || alpha > zeta {
		t.Errorf("samples are not in label order:\n%s", out.String())
	}
}

func TestWriter_EscapesLabelValues(t *testing.T) {
	t.Parallel()

	var out Writer

	out.GaugeVec("processd_processes_running", "attempts", []Sample{
		{Labels: []Label{{Name: "worker", Value: `we"ird\`}}, Value: 1},
	})

	want := `processd_processes_running{worker="we\"ird\\"} 1` + "\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("label value was not escaped, got:\n%s", out.String())
	}
}
