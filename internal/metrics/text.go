// Package metrics accumulates what the daemon reports on /v1/metrics and
// renders it in the Prometheus text exposition format.
//
// The format is written by hand on purpose: the daemon exposes a few families,
// and a client library would add a dependency tree larger than this program to
// print them (docs/SPEC.md §18).
package metrics

import (
	"fmt"
	"slices"
	"strings"
)

// ContentType is the media type of the Prometheus text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Label is one dimension of a sample.
type Label struct {
	Name  string
	Value string
}

// Sample is one labelled value of a metric family.
type Sample struct {
	Labels []Label
	Value  float64
}

// Writer renders metric families. Samples of one family are emitted together
// and in a stable order: Prometheus tolerates neither interleaved families nor
// a series that changes position between scrapes for no reason.
type Writer struct {
	out strings.Builder
}

// Gauge writes a single unlabelled gauge.
func (w *Writer) Gauge(name, help string, value float64) {
	w.header(name, help, "gauge")
	w.sample(name, nil, value)
}

// GaugeVec writes a labelled gauge family. An empty family still emits its
// header, so a scrape can tell "no series" from "metric unknown".
func (w *Writer) GaugeVec(name, help string, samples []Sample) {
	w.vec(name, help, "gauge", samples)
}

// CounterVec writes a labelled counter family.
func (w *Writer) CounterVec(name, help string, samples []Sample) {
	w.vec(name, help, "counter", samples)
}

func (w *Writer) vec(name, help, kind string, samples []Sample) {
	w.header(name, help, kind)

	ordered := slices.Clone(samples)
	slices.SortFunc(ordered, func(a, b Sample) int {
		return strings.Compare(formatLabels(a.Labels), formatLabels(b.Labels))
	})

	for _, sample := range ordered {
		w.sample(name, sample.Labels, sample.Value)
	}
}

func (w *Writer) header(name, help, kind string) {
	fmt.Fprintf(&w.out, "# HELP %s %s\n", name, help)
	fmt.Fprintf(&w.out, "# TYPE %s %s\n", name, kind)
}

func (w *Writer) sample(name string, labels []Label, value float64) {
	fmt.Fprintf(&w.out, "%s%s %g\n", name, formatLabels(labels), value)
}

// String returns everything written so far.
func (w *Writer) String() string { return w.out.String() }

// formatLabels renders a label set, or the empty string when there is none.
func formatLabels(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}

	var out strings.Builder

	out.WriteByte('{')

	for i, label := range labels {
		if i > 0 {
			out.WriteByte(',')
		}

		fmt.Fprintf(&out, `%s="%s"`, label.Name, escapeValue(label.Value))
	}

	out.WriteByte('}')

	return out.String()
}

// escapeValue escapes the three characters the exposition format reserves
// inside a label value.
var escapeValue = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace
