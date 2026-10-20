package exporter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func putFile(t *testing.T, root, path, content string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func gather(t *testing.T, c *Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, family := range families {
		out[family.GetName()] = family
	}
	return out
}

func gauge(t *testing.T, families map[string]*dto.MetricFamily, name string, want float64) {
	t.Helper()
	family := families["node_memory_tier_"+name]
	if family == nil || len(family.Metric) != 1 || family.GetType() != dto.MetricType_GAUGE || family.Metric[0].GetGauge().GetValue() != want {
		t.Fatalf("%s: got %v, want one gauge %v", name, family, want)
	}
}

func TestAnonIncludesTHPRegardlessOfOrder(t *testing.T) {
	for _, content := range []string{
		"Node 0 AnonPages: 3145728 kB\nNode 0 AnonHugePages: 2097152 kB\n",
		"Node 0 AnonHugePages: 2097152 kB\nNode 0 AnonPages: 3145728 kB\n",
	} {
		root := t.TempDir()
		putFile(t, root, "devices/system/node/node0/meminfo", content)
		m, err := ReadNodeMeminfo(root, 0)
		if err != nil || m.Anon != 3<<30 || m.THP != 2<<30 {
			t.Fatalf("meminfo = %+v, %v", m, err)
		}
	}
}

func TestIncompleteTierDiscovery(t *testing.T) {
	t.Run("empty interface", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "devices/virtual/memory_tiering"), 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadTierMap(root); err == nil {
			t.Fatal("empty interface accepted as absent")
		}
	})
	t.Run("duplicate membership", func(t *testing.T) {
		root := t.TempDir()
		putFile(t, root, "devices/virtual/memory_tiering/memory_tier4/nodelist", "0")
		putFile(t, root, "devices/virtual/memory_tiering/memory_tier22/nodelist", "0")
		if _, err := ReadTierMap(root); err == nil {
			t.Fatal("duplicate membership accepted")
		}
	})
	for _, tc := range []struct{ name, nodelist string }{
		{"missing", ""}, {"malformed", "oops"}, {"reversed", "3-1"}, {"negative", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			putFile(t, root, "devices/virtual/memory_tiering/memory_tier4/nodelist", "0")
			path := "devices/virtual/memory_tiering/memory_tier22/nodelist"
			putFile(t, root, path, tc.nodelist)
			if tc.name == "missing" {
				if err := os.Remove(filepath.Join(root, path)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadTierMap(root); err == nil {
				t.Fatal("incomplete topology accepted")
			}
		})
	}
	t.Run("unclassified live node", func(t *testing.T) {
		root := t.TempDir()
		putFile(t, root, "devices/virtual/memory_tiering/memory_tier4/nodelist", "0")
		putFile(t, root, "devices/system/node/node0/meminfo", "Node 0 MemTotal: 1024 kB")
		putFile(t, root, "devices/system/node/node1/meminfo", "Node 1 MemTotal: 1024 kB")
		c := newTestCollector(&fakeLister{})
		c.opts.SysfsRoot = root
		families := gather(t, c)
		gauge(t, families, "scrape_success", 0)
		gauge(t, families, "workload_attribution_error", 1)
		if families["node_memory_tier_nodes"] != nil {
			t.Fatal("partial topology emitted tier membership")
		}
		if tier, rank := c.nodeTierInfo(TierMap{0: 4}, map[int]int{0: 0}, 1); tier != -1 || rank != -1 {
			t.Fatal("unclassified node assigned a known tier")
		}
		if tier, rank := c.nodeTierInfo(nil, nil, 1); tier != 0 || rank != 0 {
			t.Fatal("absent interface lost fallback")
		}
	})
}

func TestPodCgroupLayouts(t *testing.T) {
	for _, tc := range []struct{ qos, path string }{
		{"Guaranteed", "kubepods/podabc-def"},
		{"Burstable", "kubepods/burstable/podabc-def"},
		{"BestEffort", "kubepods/besteffort/podabc-def"},
		{"Guaranteed", "kubepods.slice/kubepods-podabc_def.slice"},
		{"Burstable", "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podabc_def.slice"},
		{"BestEffort", "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podabc_def.slice"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root := t.TempDir()
			putFile(t, root, tc.path+"/memory.numa_stat", "anon N0=0\nfile N0=0\nshmem N0=0\n")
			got, err := podCgroupPath(root, "abc-def", tc.qos)
			if err != nil || got != filepath.Join(root, tc.path) {
				t.Fatalf("path = %q, %v", got, err)
			}
		})
	}
}

type failingLister struct{}

func (failingLister) Pods() ([]Pod, error) { return nil, errors.New("API unavailable") }

func TestAttributionHealthAndCounts(t *testing.T) {
	for _, lister := range []PodLister{nil, &fakeLister{}, failingLister{}} {
		families := gather(t, newTestCollector(lister))
		want := float64(0)
		if _, failed := lister.(failingLister); failed {
			want = 1
		}
		gauge(t, families, "workload_attribution_error", want)
		gauge(t, families, "scrape_success", 1)
	}
	// Health is a per-pass gauge, so recovery must clear a prior failure.
	recovering := newTestCollector(failingLister{})
	gauge(t, gather(t, recovering), "workload_attribution_error", 1)
	recovering.opts.PodLister = &fakeLister{}
	gauge(t, gather(t, recovering), "workload_attribution_error", 0)
	root := t.TempDir()
	valid := "anon N0=4096\nfile N0=0\nshmem N0=0\n"
	putFile(t, root, "kubepods/podok/memory.numa_stat", valid)
	putFile(t, root, "kubepods/podinvalid/memory.numa_stat", "invalid data")
	putFile(t, root, "kubepods/podvanished/placeholder", "")
	pods := []Pod{}
	for _, uid := range []string{"ok", "missing", "invalid", "vanished"} {
		pods = append(pods, Pod{Name: uid, UID: uid, NodeName: "worker-test"})
	}
	pods = append(pods, Pod{Name: "remote", UID: "ok", NodeName: "other-node"})
	c := newTestCollector(&fakeLister{pods})
	c.opts.CgroupfsRoot = root
	families := gather(t, c)
	gauge(t, families, "workload_pods_discovered", 4)
	gauge(t, families, "workload_pods_read", 1)
	gauge(t, families, "workload_attribution_error", 1)
	for _, metric := range families["node_memory_tier_workload_pods_skipped"].Metric {
		if metric.GetGauge().GetValue() != 1 {
			t.Fatalf("skip counts = %v", metric)
		}
	}
	// An actual filesystem read error (directory in place of the file) is
	// distinct from a vanished file, independent of whether tests run as root.
	if err := os.Remove(filepath.Join(root, "kubepods/podinvalid/memory.numa_stat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "kubepods/podinvalid/memory.numa_stat"), 0755); err != nil {
		t.Fatal(err)
	}
	gauge(t, gather(t, c), "workload_attribution_error", 1)
}

func TestScrapeErrorsIncrementBeforeEmission(t *testing.T) {
	c := newTestCollector(nil)
	c.opts.SysfsRoot = t.TempDir()
	for i := 1; i <= 2; i++ {
		families := gather(t, c)
		if got := families["node_memory_tier_scrape_errors_total"].Metric[0].GetCounter().GetValue(); got != float64(i) {
			t.Fatalf("scrape errors = %v, want %d", got, i)
		}
		gauge(t, families, "workload_attribution_error", 0)
	}
}

func TestParseHugetlbNumaStat(t *testing.T) {
	for _, content := range []string{"total=1073741824 N0=1073741824 N1=0\n", "total=0 N0=0 N1=0\n"} {
		nodes, err := ParseHugetlbNumaStat(content)
		if err != nil || len(nodes) != 2 || nodes[1] != 0 {
			t.Fatalf("parse = %v, %v", nodes, err)
		}
	}
	for _, content := range []string{"", "1073741824", "total=1", "total=-1 N0=0", "total=0 N0=-1", "total=0 Nbad=0", "total=0 N0=x", "total=0 N0=0 N0=0", "total=0 N0=0\nhierarchical_total=0 N0=0"} {
		if _, err := ParseHugetlbNumaStat(content); err == nil {
			t.Errorf("accepted malformed %q", content)
		}
	}
}

func TestHugetlbResidencyAndAvailability(t *testing.T) {
	for _, tc := range []struct {
		name, content      string
		available, failure float64
		wantBytes          bool
	}{
		{"absent", "", 0, 0, false},
		{"current_only", "", 0, 0, false},
		{"valid", "total=1073741824 N0=1073741824 N1=0\n", 1, 0, true},
		{"zero", "total=0 N0=0 N1=0\n", 1, 0, true},
		{"malformed", "anon N0=1073741824", 0, 1, false},
		{"read_error", "", 0, 1, false},
		{"partial_sizes", "total=1073741824 N0=1073741824 N1=0\n", 0, 0, true},
		{"multiple_sizes", "total=1073741824 N0=1073741824 N1=0\n", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cg := "kubepods/podtest"
			putFile(t, root, cg+"/memory.numa_stat", "anon N0=4096\nfile N0=0\nshmem N0=0\n")
			if tc.name != "absent" {
				putFile(t, root, cg+"/hugetlb.2MB.current", "1073741824")
			}
			if tc.content != "" {
				putFile(t, root, cg+"/hugetlb.2MB.numa_stat", tc.content)
			}
			if tc.name == "read_error" {
				if err := os.Mkdir(filepath.Join(root, cg, "hugetlb.2MB.numa_stat"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "partial_sizes" {
				putFile(t, root, cg+"/hugetlb.1GB.current", "1073741824")
			}
			if tc.name == "multiple_sizes" {
				putFile(t, root, cg+"/hugetlb.1GB.numa_stat", "total=2147483648 N0=0 N1=2147483648\n")
			}
			// A container's copy must not be added to its pod-parent value.
			putFile(t, root, cg+"/container/hugetlb.2MB.numa_stat", "total=1073741824 N0=1073741824 N1=0")
			c := newTestCollector(&fakeLister{[]Pod{{Name: "test", UID: "test", OwnerKind: "VirtualMachineInstance", OwnerName: "vm"}}})
			c.opts.CgroupfsRoot = root
			families := gather(t, c)
			gauge(t, families, "workload_hugetlb_numa_available", tc.available)
			gauge(t, families, "workload_attribution_error", tc.failure)
			family := families["node_memory_tier_workload_hugetlb_bytes"]
			if (family != nil) != tc.wantBytes {
				t.Fatalf("unexpected hugetlb series: %v", family)
			}
			if tc.wantBytes {
				wantSeries := 2
				if tc.name == "multiple_sizes" {
					wantSeries = 4
				}
				if len(family.Metric) != wantSeries {
					t.Fatal("expected only pod-parent NUMA nodes")
				}
				var total float64
				for _, metric := range family.Metric {
					total += metric.GetGauge().GetValue()
					labels := map[string]string{}
					for _, label := range metric.Label {
						labels[label.GetName()] = label.GetValue()
					}
					if (labels["page_size"] != "2MB" && labels["page_size"] != "1GB") || labels["workload"] != "vm" || labels["tier"] == "-1" {
						t.Fatalf("wrong labels: %v", labels)
					}
				}
				want := float64(1 << 30)
				if tc.name == "multiple_sizes" {
					want = 3 << 30
				}
				if strings.HasPrefix(tc.content, "total=0 ") {
					want = 0
				}
				if total != want {
					t.Fatalf("hugetlb bytes %v, want %v", total, want)
				}
			}
		})
	}
}
