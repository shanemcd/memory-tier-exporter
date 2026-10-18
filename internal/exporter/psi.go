package exporter

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var psiStalledDesc = prometheus.NewDesc(
	"node_memory_tier_psi_stalled_seconds_total",
	"Cumulative time some (at least one) or all tasks were stalled on memory, from /proc/pressure/memory. The kernel reports totals in microseconds; this metric converts to seconds.",
	[]string{"node", "scope"}, nil,
)

var psiAvgDesc = prometheus.NewDesc(
	"node_memory_tier_psi_stalled_ratio",
	"Fraction of the last 10/60/300 seconds during which some or all tasks were stalled on memory, from /proc/pressure/memory (the kernel's avg10/avg60/avg300 percentages, expressed as a ratio).",
	[]string{"node", "scope", "window"}, nil,
)

// collectPSI parses /proc/pressure/memory.
func (c *Collector) collectPSI(ch chan<- prometheus.Metric) error {
	f, err := os.Open(filepath.Join(c.opts.ProcfsRoot, "pressure", "memory"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // kernel without PSI
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// "some avg10=0.00 avg60=0.00 avg300=0.00 total=0"
		fields := strings.Fields(sc.Text())
		if len(fields) != 5 {
			continue
		}
		scope := fields[0]
		if scope != "some" && scope != "full" {
			continue
		}
		kv := map[string]string{}
		for _, x := range fields[1:] {
			parts := strings.SplitN(x, "=", 2)
			if len(parts) == 2 {
				kv[parts[0]] = parts[1]
			}
		}
		if total, err := strconv.ParseFloat(kv["total"], 64); err == nil {
			// /proc/pressure totals are microseconds.
			ch <- prometheus.MustNewConstMetric(psiStalledDesc, prometheus.CounterValue, total/1e6, c.opts.NodeName, scope)
		}
		for _, window := range []string{"10", "60", "300"} {
			if avg, err := strconv.ParseFloat(kv["avg"+window], 64); err == nil {
				ch <- prometheus.MustNewConstMetric(psiAvgDesc, prometheus.GaugeValue, avg/100, c.opts.NodeName, scope, window)
			}
		}
	}
	return sc.Err()
}
