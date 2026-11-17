// Package metrics is a small, dependency-free Prometheus text-format
// exporter for the router's counters, summaries and live gauges.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds every metric. Safe for concurrent use.
type Registry struct {
	mu        sync.Mutex
	families  map[string]*family
	gaugeFunc []gaugeFunc
}

type family struct {
	name, help, kind string
	labels           []string
	series           map[string]*series
}

type series struct {
	labelValues []string
	value       float64 // counter
	sum         float64 // summary
	count       uint64  // summary
}

type gaugeFunc struct {
	name, help string
	labels     []string
	collect    func() []Sample
}

// Sample is one gauge value with its label values.
type Sample struct {
	LabelValues []string
	Value       float64
}

// New returns an empty registry.
func New() *Registry { return &Registry{families: map[string]*family{}} }

// Counter is a labelled monotonically increasing counter.
type Counter struct {
	r *Registry
	f *family
}

// Summary records sums and counts of observations (in seconds).
type Summary struct {
	r *Registry
	f *family
}

// NewCounter registers a counter family.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	return &Counter{r, r.register(name, help, "counter", labels)}
}

// NewSummary registers a summary family (sum and count only).
func (r *Registry) NewSummary(name, help string, labels ...string) *Summary {
	return &Summary{r, r.register(name, help, "summary", labels)}
}

// GaugeFunc registers a gauge whose samples are collected at scrape time.
func (r *Registry) GaugeFunc(name, help string, labels []string, collect func() []Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gaugeFunc = append(r.gaugeFunc, gaugeFunc{name, help, labels, collect})
}

func (r *Registry) register(name, help, kind string, labels []string) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.families[name]; ok {
		panic("metrics: duplicate metric " + name)
	}
	f := &family{name: name, help: help, kind: kind, labels: labels, series: map[string]*series{}}
	r.families[name] = f
	return f
}

func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d labels, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	s, ok := f.series[key]
	if !ok {
		s = &series{labelValues: append([]string(nil), values...)}
		f.series[key] = s
	}
	return s
}

// Inc adds one to the series with the given label values.
func (c *Counter) Inc(values ...string) {
	c.r.mu.Lock()
	c.f.get(values).value++
	c.r.mu.Unlock()
}

// Observe records a duration in the series with the given label values.
func (s *Summary) Observe(d time.Duration, values ...string) {
	s.r.mu.Lock()
	ser := s.f.get(values)
	ser.sum += d.Seconds()
	ser.count++
	s.r.mu.Unlock()
}

// Write renders every metric in Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		f := r.families[n]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			lbl := labelString(f.labels, s.labelValues)
			if f.kind == "summary" {
				fmt.Fprintf(&b, "%s_sum%s %s\n%s_count%s %d\n", f.name, lbl, num(s.sum), f.name, lbl, s.count)
			} else {
				fmt.Fprintf(&b, "%s%s %s\n", f.name, lbl, num(s.value))
			}
		}
	}
	gauges := append([]gaugeFunc(nil), r.gaugeFunc...)
	r.mu.Unlock()

	// Collect gauges outside the lock: collectors may take their own locks.
	for _, g := range gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, escapeHelp(g.help), g.name)
		samples := g.collect()
		sort.Slice(samples, func(i, j int) bool {
			return strings.Join(samples[i].LabelValues, "\xff") < strings.Join(samples[j].LabelValues, "\xff")
		})
		for _, s := range samples {
			fmt.Fprintf(&b, "%s%s %s\n", g.name, labelString(g.labels, s.LabelValues), num(s.Value))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func labelString(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func num(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
