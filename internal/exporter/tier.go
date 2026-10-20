package exporter

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Tier metrics carry four labels:
//
//	node      the machine's Kubernetes node name (join key with other
//	          node- and VM-scoped metrics)
//	numa_node the NUMA node id within the machine ("0", "1", ...)
//	tier      the kernel memory-tier number, derived by the kernel from the
//	          memory's abstract distance (smaller is faster)
//	rank      the zero-based position of the tier among the tiers present on
//	          this machine; rank 0 is the fastest tier present (CPU-attached
//	          DRAM on current platforms)
var tierMetricLabels = []string{"node", "numa_node", "tier", "rank"}

var tierNodesDesc = prometheus.NewDesc(
	"node_memory_tier_nodes",
	"Memory-tier membership of each NUMA node (always 1). tier is the kernel memory-tier number derived from abstract distance (smaller is faster); rank is the zero-based position among the tiers present on this machine, so rank 0 is the fastest tier present (CPU-attached DRAM on current platforms).",
	tierMetricLabels, nil,
)

// TierMap maps NUMA node id -> sysfs tier number. A missing entry means the
// node was not listed in any tier (unexpected on tiering-capable kernels).
type TierMap map[int]int

// ReadTierMap parses /sys/devices/virtual/memory_tiering/memory_tierN/nodelist.
// It returns (nil, nil) when the kernel does not expose memory tiers.
func ReadTierMap(sysfsRoot string) (TierMap, error) {
	base := filepath.Join(sysfsRoot, "devices", "virtual", "memory_tiering")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	tm := TierMap{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "memory_tier") || !e.IsDir() {
			continue
		}
		tier, err := strconv.Atoi(strings.TrimPrefix(name, "memory_tier"))
		if err != nil || tier < 0 {
			return nil, fmt.Errorf("invalid tier directory %q", name)
		}
		data, err := os.ReadFile(filepath.Join(base, name, "nodelist"))
		if err != nil {
			return nil, fmt.Errorf("read %s nodelist: %w", name, err)
		}
		nodes, err := parseRangeList(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("parse %s nodelist: %w", name, err)
		}
		for _, n := range nodes {
			if _, exists := tm[n]; exists {
				return nil, fmt.Errorf("duplicate tier membership for node %d", n)
			}
			tm[n] = tier
		}
	}
	if len(tm) == 0 {
		return nil, fmt.Errorf("memory-tier interface exists but has no nodes")
	}
	return tm, nil
}

// Ranks returns NUMA node id -> rank (0 = fastest tier present).
func (tm TierMap) Ranks() map[int]int {
	if len(tm) == 0 {
		return nil
	}
	tiers := map[int]bool{}
	for _, t := range tm {
		tiers[t] = true
	}
	sorted := make([]int, 0, len(tiers))
	for t := range tiers {
		sorted = append(sorted, t)
	}
	sort.Ints(sorted)
	rankOf := map[int]int{}
	for i, t := range sorted {
		rankOf[t] = i
	}
	ranks := map[int]int{}
	for n, t := range tm {
		ranks[n] = rankOf[t]
	}
	return ranks
}

// parseRangeList parses kernel range lists like "0", "0-3", "0-3,7".
func parseRangeList(s string) ([]int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty list")
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			lo, err1 := strconv.Atoi(bounds[0])
			hi, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil || lo < 0 || hi < lo {
				return nil, fmt.Errorf("bad range %q", part)
			}
			for i := lo; i <= hi; i++ {
				out = append(out, i)
			}
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("bad value %q", part)
		}
		out = append(out, v)
	}
	return out, nil
}

// listNUMANodes returns the memory-bearing NUMA node ids present in sysfs.
func listNUMANodes(sysfsRoot string) ([]int, error) {
	base := filepath.Join(sysfsRoot, "devices", "system", "node")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(base, "has_memory"))
	var memoryNodes map[int]bool
	if err == nil {
		memoryNodes = map[int]bool{}
		if list := strings.TrimSpace(string(data)); list != "" {
			nodes, err := parseRangeList(list)
			if err != nil {
				return nil, fmt.Errorf("parse has_memory: %w", err)
			}
			for _, n := range nodes {
				memoryNodes[n] = true
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var nodes []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "node") || !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(name, "node"))
		if err != nil {
			continue
		}
		if memoryNodes != nil {
			if !memoryNodes[n] {
				continue
			}
		} else {
			// Older kernels may not expose has_memory. CPU-only nodes
			// have zero MemTotal and need no memory-tier membership.
			mem, err := ReadNodeMeminfo(sysfsRoot, n)
			if err != nil {
				return nil, err
			}
			if mem.Total == 0 {
				continue
			}
		}
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	return nodes, nil
}

// NodeMem is the per-node memory usage in bytes parsed from node meminfo.
// Anon and File are the anonymous and file-backed page counts; they matter
// because "used" includes reclaimable page cache, while anonymous memory is
// what demotion moves between tiers.
type NodeMem struct {
	Total int64
	Free  int64
	Used  int64
	Anon  int64 // AnonPages already includes anonymous THP on Linux 6.12.
	File  int64
	THP   int64
}

// ReadNodeMeminfo parses MemTotal, MemFree and MemUsed from
// /sys/devices/system/node/nodeN/meminfo (sysfs reports kB; bytes returned).
func ReadNodeMeminfo(sysfsRoot string, node int) (NodeMem, error) {
	path := filepath.Join(sysfsRoot, "devices", "system", "node", fmt.Sprintf("node%d", node), "meminfo")
	f, err := os.Open(path)
	if err != nil {
		return NodeMem{}, err
	}
	defer f.Close()
	var m NodeMem
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// "Node 1 MemTotal:        4194304 kB"
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[0] != "Node" {
			continue
		}
		if n, err := strconv.Atoi(fields[1]); err != nil || n != node {
			continue
		}
		key := strings.TrimSuffix(fields[2], ":")
		v, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		bytes := v * 1024 // sysfs node meminfo is always kB
		switch key {
		case "MemTotal":
			m.Total = bytes
		case "MemFree":
			m.Free = bytes
		case "MemUsed":
			m.Used = bytes
		case "AnonPages":
			m.Anon = bytes
		case "FilePages":
			m.File = bytes
		case "AnonHugePages":
			m.THP = bytes
		}
	}
	return m, sc.Err()
}

type tierMemKind int

const (
	tierMemTotal tierMemKind = iota
	tierMemFree
	tierMemUsed
)

func (k tierMemKind) name() string {
	switch k {
	case tierMemTotal:
		return "total"
	case tierMemFree:
		return "free"
	default:
		return "used"
	}
}

func tierMemDesc(kind tierMemKind) *prometheus.Desc {
	return prometheus.NewDesc(
		fmt.Sprintf("node_memory_tier_mem%s_bytes", kind.name()),
		fmt.Sprintf("Memory %s on this NUMA node, labeled by memory tier (parsed from /sys/devices/system/node/nodeN/meminfo).", kind.name()),
		tierMetricLabels, nil,
	)
}

var tierAnonDesc = prometheus.NewDesc(
	"node_memory_tier_anon_bytes",
	"Anonymous memory on this NUMA node, labeled by memory tier (node meminfo AnonPages). Anonymous memory is what kernel tiering demotes and promotes; it approximates a workload's resident working set.",
	tierMetricLabels, nil,
)

var tierAnonTHPDesc = prometheus.NewDesc(
	"node_memory_tier_anon_thp_bytes",
	"Anonymous transparent-hugepage memory on this NUMA node, labeled by memory tier (node meminfo AnonHugePages). Included in anon_bytes, not additive; excludes explicit HugeTLB guest RAM.",
	tierMetricLabels, nil,
)

var tierFileDesc = prometheus.NewDesc(
	"node_memory_tier_file_bytes",
	"File-backed page cache on this NUMA node, labeled by memory tier (node meminfo FilePages). Includes shmem/tmpfs and dirty or mapped pages; not all of it is immediately discardable.",
	tierMetricLabels, nil,
)

// NodeTierInfo resolves the tier and rank labels for one NUMA node.
// Kernels without the memory-tier sysfs tree get a synthesized tier 0 /
// rank 0 so DRAM-only machines still report useful series.
func (c *Collector) nodeTierInfo(tm TierMap, ranks map[int]int, n int) (tier, rank int) {
	if t, ok := tm[n]; ok {
		return t, ranks[n]
	}
	if tm == nil {
		return 0, 0
	}
	// An unclassified node must never appear to be the fastest tier.
	return -1, -1
}

// collectTiers emits the tier mapping and per-NUMA-node memory metrics.
func (c *Collector) collectTiers(ch chan<- prometheus.Metric, tm TierMap, ranks map[int]int) error {
	nodes, err := listNUMANodes(c.opts.SysfsRoot)
	if err != nil {
		return err
	}
	// Validate the complete discovery before emitting any tier membership.
	if tm != nil {
		for _, n := range nodes {
			if _, ok := tm[n]; !ok {
				return fmt.Errorf("NUMA node %d missing from memory-tier interface", n)
			}
		}
	}
	for _, n := range nodes {
		tier, rank := c.nodeTierInfo(tm, ranks, n)
		labels := []string{
			c.opts.NodeName,
			strconv.Itoa(n),
			strconv.Itoa(tier),
			strconv.Itoa(rank),
		}
		ch <- prometheus.MustNewConstMetric(tierNodesDesc, prometheus.GaugeValue, 1, labels...)

		mem, err := ReadNodeMeminfo(c.opts.SysfsRoot, n)
		if err != nil {
			return err
		}
		ch <- prometheus.MustNewConstMetric(tierMemDesc(tierMemTotal), prometheus.GaugeValue, float64(mem.Total), labels...)
		ch <- prometheus.MustNewConstMetric(tierMemDesc(tierMemFree), prometheus.GaugeValue, float64(mem.Free), labels...)
		ch <- prometheus.MustNewConstMetric(tierMemDesc(tierMemUsed), prometheus.GaugeValue, float64(mem.Used), labels...)
		ch <- prometheus.MustNewConstMetric(tierAnonDesc, prometheus.GaugeValue, float64(mem.Anon), labels...)
		ch <- prometheus.MustNewConstMetric(tierAnonTHPDesc, prometheus.GaugeValue, float64(mem.THP), labels...)
		ch <- prometheus.MustNewConstMetric(tierFileDesc, prometheus.GaugeValue, float64(mem.File), labels...)
	}
	return nil
}
