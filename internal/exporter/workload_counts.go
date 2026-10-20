package exporter

import "github.com/prometheus/client_golang/prometheus"

var workloadDiscoveredDesc = prometheus.NewDesc(
	"node_memory_tier_workload_pods_discovered", "Selected local pods discovered in the last attribution pass (0 if disabled or listing failed).", []string{"node"}, nil)
var workloadReadDesc = prometheus.NewDesc(
	"node_memory_tier_workload_pods_read", "Pods with ordinary memory.numa_stat successfully read in the last attribution pass; HugeTLB coverage is separate.", []string{"node"}, nil)
var workloadSkippedDesc = prometheus.NewDesc(
	"node_memory_tier_workload_pods_skipped", "Pods skipped in the last attribution pass by bounded reason: not_found (no supported cgroup path), disappeared (file vanished), read_error (I/O or invalid data).", []string{"node", "reason"}, nil)

type workloadCounts struct {
	discovered int
	read       int
	skipped    map[string]int
}

func (s *workloadCounts) emit(ch chan<- prometheus.Metric, node string) {
	ch <- prometheus.MustNewConstMetric(workloadDiscoveredDesc, prometheus.GaugeValue, float64(s.discovered), node)
	ch <- prometheus.MustNewConstMetric(workloadReadDesc, prometheus.GaugeValue, float64(s.read), node)
	for _, reason := range []string{"not_found", "disappeared", "read_error"} {
		ch <- prometheus.MustNewConstMetric(workloadSkippedDesc, prometheus.GaugeValue, float64(s.skipped[reason]), node, reason)
	}
}
