package exporter

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

const testdata = "../../testdata"

type fakeLister struct{ pods []Pod }

func (f *fakeLister) LauncherPods() ([]Pod, error) { return f.pods, nil }

func newTestCollector(lister PodLister) *Collector {
	return NewCollector(Options{
		NodeName:     "worker-test",
		SysfsRoot:    testdata + "/sys",
		ProcfsRoot:   testdata + "/proc",
		CgroupfsRoot: testdata + "/cgroup",
		PodLister:    lister,
	})
}

func TestReadTierMap(t *testing.T) {
	tm, err := ReadTierMap(testdata + "/sys")
	if err != nil {
		t.Fatal(err)
	}
	if tm[0] != 4 {
		t.Errorf("node0 tier = %d, want 4", tm[0])
	}
	if tm[1] != 22 {
		t.Errorf("node1 tier = %d, want 22", tm[1])
	}
	ranks := tm.Ranks()
	if ranks[0] != 0 || ranks[1] != 1 {
		t.Errorf("ranks = %v, want node0=0 node1=1", ranks)
	}
}

func TestReadTierMapAbsent(t *testing.T) {
	tm, err := ReadTierMap(testdata + "/nonexistent")
	if err != nil {
		t.Fatalf("absent tierfs should not error: %v", err)
	}
	if tm != nil {
		t.Errorf("absent tierfs should return nil, got %v", tm)
	}
}

func TestParseRangeList(t *testing.T) {
	for in, want := range map[string]int{
		"0": 1, "0-3": 4, "0-3,7": 5, "1,3,5": 3,
	} {
		got, err := parseRangeList(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if len(got) != want {
			t.Errorf("%q: got %d nodes, want %d", in, len(got), want)
		}
	}
	if _, err := parseRangeList(""); err == nil {
		t.Error("empty list should error")
	}
}

func TestReadNodeMeminfo(t *testing.T) {
	m, err := ReadNodeMeminfo(testdata+"/sys", 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 4194304*1024 || m.Free != 4182652*1024 || m.Used != 11652*1024 {
		t.Errorf("meminfo = %+v", m)
	}
}

func TestParseNumaStat(t *testing.T) {
	stat := ParseNumaStat("anon N0=160829440 N1=4096\nfile N0=598016\n")
	if stat["anon"][0] != 160829440 || stat["anon"][1] != 4096 {
		t.Errorf("anon = %v", stat["anon"])
	}
	if stat["file"][1] != 0 {
		t.Errorf("missing N1 should be absent, got %v", stat["file"])
	}
}

// TestCollectorEndToEnd scrapes the collector against fixtures and checks the
// exposed series.
func TestCollectorEndToEnd(t *testing.T) {
	lister := &fakeLister{pods: []Pod{{
		Namespace: "default",
		Name:      "virt-launcher-demo-abcde",
		UID:       "11111111-2222-3333-4444-555555555555",
		QOSClass:  "Burstable",
		VMIName:   "demo",
		NodeName:  "worker-test",
	}}}
	c := newTestCollector(lister)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	expected := `
# HELP node_memory_tier_nodes Memory-tier membership of each NUMA node (always 1). tier is the kernel memory-tier number derived from abstract distance (smaller is faster); rank is the zero-based position among the tiers present on this machine, so rank 0 is the fastest tier present (CPU-attached DRAM on current platforms).
# TYPE node_memory_tier_nodes gauge
node_memory_tier_nodes{node="worker-test",numa_node="0",rank="0",tier="4"} 1
node_memory_tier_nodes{node="worker-test",numa_node="1",rank="1",tier="22"} 1
# HELP node_memory_tier_anon_bytes Anonymous memory on this NUMA node, labeled by memory tier (node meminfo AnonPages). Anonymous memory is what kernel tiering demotes and promotes; it approximates a workload's resident working set.
# TYPE node_memory_tier_anon_bytes gauge
node_memory_tier_anon_bytes{node="worker-test",numa_node="0",rank="0",tier="4"} 3.072e+09
node_memory_tier_anon_bytes{node="worker-test",numa_node="1",rank="1",tier="22"} 1.1931648e+07
# HELP node_memory_tier_anon_thp_bytes Anonymous transparent-hugepage memory on this NUMA node, labeled by memory tier (node meminfo AnonHugePages). Included in anon_bytes; virtualization guests typically allocate their memory as THP.
# TYPE node_memory_tier_anon_thp_bytes gauge
node_memory_tier_anon_thp_bytes{node="worker-test",numa_node="0",rank="0",tier="4"} 2.048e+09
node_memory_tier_anon_thp_bytes{node="worker-test",numa_node="1",rank="1",tier="22"} 0
# HELP node_memory_tier_file_bytes File-backed page cache on this NUMA node, labeled by memory tier (node meminfo FilePages). This memory is reclaimable without tiering.
# TYPE node_memory_tier_file_bytes gauge
node_memory_tier_file_bytes{node="worker-test",numa_node="0",rank="0",tier="4"} 4.096e+09
node_memory_tier_file_bytes{node="worker-test",numa_node="1",rank="1",tier="22"} 0
# HELP node_memory_tier_memfree_bytes Memory free on this NUMA node, labeled by memory tier (parsed from /sys/devices/system/node/nodeN/meminfo).
# TYPE node_memory_tier_memfree_bytes gauge
node_memory_tier_memfree_bytes{node="worker-test",numa_node="0",rank="0",tier="4"} 1.264196608e+09
node_memory_tier_memfree_bytes{node="worker-test",numa_node="1",rank="1",tier="22"} 4.283035648e+09
# HELP node_memory_tier_memtotal_bytes Memory total on this NUMA node, labeled by memory tier (parsed from /sys/devices/system/node/nodeN/meminfo).
# TYPE node_memory_tier_memtotal_bytes gauge
node_memory_tier_memtotal_bytes{node="worker-test",numa_node="0",rank="0",tier="4"} 8.589934592e+09
node_memory_tier_memtotal_bytes{node="worker-test",numa_node="1",rank="1",tier="22"} 4.294967296e+09
# HELP node_memory_tier_memused_bytes Memory used on this NUMA node, labeled by memory tier (parsed from /sys/devices/system/node/nodeN/meminfo).
# TYPE node_memory_tier_memused_bytes gauge
node_memory_tier_memused_bytes{node="worker-test",numa_node="0",rank="0",tier="4"} 7.325737984e+09
node_memory_tier_memused_bytes{node="worker-test",numa_node="1",rank="1",tier="22"} 1.1931648e+07
# HELP node_memory_tier_pgdemote_total Cumulative pages demoted from a faster to a slower memory tier, by reclaim path (from /proc/vmstat pgdemote_*).
# TYPE node_memory_tier_pgdemote_total counter
node_memory_tier_pgdemote_total{mode="direct",node="worker-test"} 194624
node_memory_tier_pgdemote_total{mode="khugepaged",node="worker-test"} 0
node_memory_tier_pgdemote_total{mode="kswapd",node="worker-test"} 0
# HELP node_memory_tier_pgpromote_total Cumulative pages promoted toward a faster memory tier, by outcome (from /proc/vmstat pgpromote_*).
# TYPE node_memory_tier_pgpromote_total counter
node_memory_tier_pgpromote_total{mode="candidate",node="worker-test"} 30
node_memory_tier_pgpromote_total{mode="candidate_nrl",node="worker-test"} 12
node_memory_tier_pgpromote_total{mode="success",node="worker-test"} 17
# HELP node_memory_tier_psi_stalled_seconds_total Cumulative time some (at least one) or all tasks were stalled on memory, from /proc/pressure/memory. The kernel reports totals in microseconds; this metric converts to seconds.
# TYPE node_memory_tier_psi_stalled_seconds_total counter
node_memory_tier_psi_stalled_seconds_total{node="worker-test",scope="full"} 2.5e-05
node_memory_tier_psi_stalled_seconds_total{node="worker-test",scope="some"} 1.234567
# HELP node_memory_tier_psi_stalled_ratio Fraction of the last 10/60/300 seconds during which some or all tasks were stalled on memory, from /proc/pressure/memory (the kernel's avg10/avg60/avg300 percentages, expressed as a ratio).
# TYPE node_memory_tier_psi_stalled_ratio gauge
node_memory_tier_psi_stalled_ratio{node="worker-test",scope="full",window="10"} 0
node_memory_tier_psi_stalled_ratio{node="worker-test",scope="full",window="300"} 0
node_memory_tier_psi_stalled_ratio{node="worker-test",scope="full",window="60"} 0
node_memory_tier_psi_stalled_ratio{node="worker-test",scope="some",window="10"} 0
node_memory_tier_psi_stalled_ratio{node="worker-test",scope="some",window="300"} 0.0001
node_memory_tier_psi_stalled_ratio{node="worker-test",scope="some",window="60"} 0.0003
# HELP node_memory_tier_vm_anon_bytes anon memory of the VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat; includes launcher overhead).
# TYPE node_memory_tier_vm_anon_bytes gauge
node_memory_tier_vm_anon_bytes{namespace="default",node="worker-test",numa_node="0",rank="0",tier="4",vmi="demo"} 1.6082944e+08
node_memory_tier_vm_anon_bytes{namespace="default",node="worker-test",numa_node="1",rank="1",tier="22",vmi="demo"} 4096
# HELP node_memory_tier_vm_pgdemote_total Cumulative pages demoted from this VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat pgdemote_* rows), by reclaim path.
# TYPE node_memory_tier_vm_pgdemote_total counter
node_memory_tier_vm_pgdemote_total{mode="direct",namespace="default",node="worker-test",numa_node="0",rank="0",tier="4",vmi="demo"} 64
node_memory_tier_vm_pgdemote_total{mode="direct",namespace="default",node="worker-test",numa_node="1",rank="1",tier="22",vmi="demo"} 0
node_memory_tier_vm_pgdemote_total{mode="kswapd",namespace="default",node="worker-test",numa_node="0",rank="0",tier="4",vmi="demo"} 0
node_memory_tier_vm_pgdemote_total{mode="kswapd",namespace="default",node="worker-test",numa_node="1",rank="1",tier="22",vmi="demo"} 0
# HELP node_memory_tier_vm_pgpromote_total Cumulative pages promoted toward a faster tier from this VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat pgpromote_* rows), by outcome.
# TYPE node_memory_tier_vm_pgpromote_total counter
node_memory_tier_vm_pgpromote_total{mode="success",namespace="default",node="worker-test",numa_node="0",rank="0",tier="4",vmi="demo"} 8
node_memory_tier_vm_pgpromote_total{mode="success",namespace="default",node="worker-test",numa_node="1",rank="1",tier="22",vmi="demo"} 0
# HELP node_memory_tier_vm_file_bytes file memory of the VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat; includes launcher overhead).
# TYPE node_memory_tier_vm_file_bytes gauge
node_memory_tier_vm_file_bytes{namespace="default",node="worker-test",numa_node="0",rank="0",tier="4",vmi="demo"} 598016
node_memory_tier_vm_file_bytes{namespace="default",node="worker-test",numa_node="1",rank="1",tier="22",vmi="demo"} 0
# HELP node_memory_tier_vm_shmem_bytes shmem memory of the VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat; includes launcher overhead).
# TYPE node_memory_tier_vm_shmem_bytes gauge
node_memory_tier_vm_shmem_bytes{namespace="default",node="worker-test",numa_node="0",rank="0",tier="4",vmi="demo"} 81920
node_memory_tier_vm_shmem_bytes{namespace="default",node="worker-test",numa_node="1",rank="1",tier="22",vmi="demo"} 0
`
	if err := testutil.GatherAndCompare(dropScrapeMetrics{reg}, strings.NewReader(expected)); err != nil {
		t.Errorf("end-to-end comparison failed:\n%v", err)
	}

	// Node label filtering: a pod on another node must not be attributed.
	lister2 := &fakeLister{pods: []Pod{{
		Namespace: "default", Name: "virt-launcher-other-abcde",
		UID: "11111111-2222-3333-4444-555555555555", QOSClass: "Burstable",
		VMIName: "other", NodeName: "worker-other",
	}}}
	c2 := newTestCollector(lister2)
	count := testutil.CollectAndCount(c2, "node_memory_tier_vm_anon_bytes")
	if count != 0 {
		t.Errorf("pod on other node produced %d vm series, want 0", count)
	}
}

// dropScrapeMetrics hides the collector's own scrape_* self-metrics (whose
// duration varies) from end-to-end comparisons.
type dropScrapeMetrics struct{ inner prometheus.Gatherer }

func (d dropScrapeMetrics) Gather() ([]*dto.MetricFamily, error) {
	mfs, err := d.inner.Gather()
	if err != nil {
		return nil, err
	}
	out := mfs[:0]
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "node_memory_tier_scrape") {
			continue
		}
		out = append(out, mf)
	}
	return out, nil
}

func TestScrapeSuccessMetric(t *testing.T) {
	c := newTestCollector(nil)
	if v := testutil.ToFloat64(c.scrapeSuccess); v != 0 {
		t.Errorf("initial success = %v, want 0", v)
	}
	// Force one scrape through the registry.
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(gathered) == 0 {
		t.Error("no metrics gathered")
	}
	if v := testutil.ToFloat64(c.scrapeSuccess); v != 1 {
		t.Errorf("after scrape success = %v, want 1", v)
	}
}
