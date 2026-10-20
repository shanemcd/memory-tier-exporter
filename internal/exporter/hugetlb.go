package exporter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var workloadHugetlbDesc = prometheus.NewDesc(
	"node_memory_tier_workload_hugetlb_bytes",
	"Explicit HugeTLB resident bytes on this NUMA node from the pod-parent hugetlb.<size>.numa_stat (hierarchical cgroup v2 usage, all containers); separate from ordinary memcg memory.",
	append(append([]string{}, workloadMetricLabels...), "page_size"), nil,
)

var workloadHugetlbAvailableDesc = prometheus.NewDesc(
	"node_memory_tier_workload_hugetlb_numa_available",
	"1 if all detected HugeTLB page sizes in this pod cgroup have readable valid NUMA residency, 0 if absent or incomplete; not a statement that the pod uses HugeTLB.",
	[]string{"node", "workload", "workload_kind", "namespace", "pod"}, nil,
)

// ParseHugetlbNumaStat parses the cgroup v2 format, in bytes:
// total=1073741824 N0=1073741824 N1=0
// The pod parent's values already include its containers. Never recurse into
// children, multiply by page size, or infer placement from hugetlb.*.current.
func ParseHugetlbNumaStat(content string) (map[int]int64, error) {
	fields := strings.Fields(content)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "total=") {
		return nil, fmt.Errorf("invalid hugetlb numa_stat header or missing nodes")
	}
	total, err := strconv.ParseInt(strings.TrimPrefix(fields[0], "total="), 10, 64)
	if err != nil || total < 0 {
		return nil, fmt.Errorf("invalid hugetlb total")
	}
	nodes := map[int]int64{}
	for _, field := range fields[1:] {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "N") {
			return nil, fmt.Errorf("invalid hugetlb node field %q", field)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(parts[0], "N"))
		v, valueErr := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n < 0 || valueErr != nil || v < 0 {
			return nil, fmt.Errorf("invalid hugetlb node field %q", field)
		}
		if _, exists := nodes[n]; exists {
			return nil, fmt.Errorf("duplicate hugetlb node %d", n)
		}
		nodes[n] = v
	}
	// Do not require total == sum(nodes): kernel counters can change during read.
	return nodes, nil
}

func (c *Collector) collectHugetlb(ch chan<- prometheus.Metric, cg string, identity []string, tm TierMap, ranks map[int]int) error {
	entries, err := os.ReadDir(cg)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(workloadHugetlbAvailableDesc, prometheus.GaugeValue, 0, identity...)
		return err
	}
	return c.collectHugetlbEntries(ch, cg, identity, tm, ranks, entries)
}

// collectHugetlbEntries keeps the discovery snapshot so a NUMA file that
// disappears before reading is an error, unlike an unsupported interface.
func (c *Collector) collectHugetlbEntries(ch chan<- prometheus.Metric, cg string, identity []string, tm TierMap, ranks map[int]int, entries []os.DirEntry) error {
	available := false
	defer func() {
		ch <- prometheus.MustNewConstMetric(workloadHugetlbAvailableDesc, prometheus.GaugeValue, btoi(available), identity...)
	}()
	// Discover sizes from either interface: current-only kernels must report
	// unavailable placement, not an invented zero or DRAM residency.
	sizes := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "hugetlb.") {
			continue
		}
		for _, suffix := range []string{".numa_stat", ".current"} {
			if strings.HasSuffix(name, suffix) {
				size := strings.TrimSuffix(strings.TrimPrefix(name, "hugetlb."), suffix)
				if size != "" && !strings.Contains(size, ".") {
					sizes[size] = sizes[size] || suffix == ".numa_stat"
				}
			}
		}
	}
	available = len(sizes) > 0
	var errs []error
	for size, numaDiscovered := range sizes {
		data, err := os.ReadFile(filepath.Join(cg, "hugetlb."+size+".numa_stat"))
		if err != nil {
			available = false
			if numaDiscovered || !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		nodes, err := ParseHugetlbNumaStat(string(data))
		if err != nil {
			available = false
			errs = append(errs, err)
			continue
		}
		for n, bytes := range nodes {
			tier, rank := c.nodeTierInfo(tm, ranks, n)
			labels := append(append([]string{}, identity...), strconv.Itoa(n), strconv.Itoa(tier), strconv.Itoa(rank), size)
			ch <- prometheus.MustNewConstMetric(workloadHugetlbDesc, prometheus.GaugeValue, float64(bytes), labels...)
		}
	}
	return errors.Join(errs...)
}
