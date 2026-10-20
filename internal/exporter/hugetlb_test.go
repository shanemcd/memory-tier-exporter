package exporter

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestHugetlbDisappearedAfterDiscovery(t *testing.T) {
	for _, discovered := range []bool{true, false} {
		name := "current_only"
		if discovered {
			name = "disappeared"
		}
		t.Run(name, func(t *testing.T) {
			cg := t.TempDir()
			putFile(t, cg, "hugetlb.2MB.current", "0\n")
			path := filepath.Join(cg, "hugetlb.2MB.numa_stat")
			if discovered {
				putFile(t, cg, "hugetlb.2MB.numa_stat", "total=0 N0=0\n")
			}
			entries, err := os.ReadDir(cg)
			if err != nil {
				t.Fatal(err)
			}
			if discovered {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			ch := make(chan prometheus.Metric, 8)
			c := newTestCollector(nil)
			err = c.collectHugetlbEntries(ch, cg, []string{"node", "vm", "VirtualMachineInstance", "default", "pod"}, nil, nil, entries)
			if discovered {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("disappeared file error = %v, want ENOENT", err)
				}
			} else if err != nil {
				t.Fatalf("current-only kernel: %v", err)
			}
			if len(ch) != 1 {
				t.Fatalf("got %d metrics, want availability only", len(ch))
			}
			var m dto.Metric
			if err := (<-ch).Write(&m); err != nil {
				t.Fatal(err)
			}
			if m.GetGauge().GetValue() != 0 {
				t.Fatal("missing NUMA residency reported as available")
			}
		})
	}
}

func TestHugetlbDiscoveredENOENTAttributionError(t *testing.T) {
	root := t.TempDir()
	cg := "kubepods/podtest"
	putFile(t, root, cg+"/memory.numa_stat", "anon N0=4096\nfile N0=0\nshmem N0=0\n")
	putFile(t, root, cg+"/hugetlb.2MB.current", "0\n")
	// A dangling symlink deterministically produces ENOENT after discovery,
	// exercising the same error path as a cgroup file disappearing mid-scrape.
	if err := os.Symlink("missing", filepath.Join(root, cg, "hugetlb.2MB.numa_stat")); err != nil {
		t.Fatal(err)
	}
	c := newTestCollector(&fakeLister{[]Pod{{Name: "test", UID: "test"}}})
	c.opts.CgroupfsRoot = root
	families := gather(t, c)
	gauge(t, families, "workload_hugetlb_numa_available", 0)
	gauge(t, families, "workload_attribution_error", 1)
	if families["node_memory_tier_workload_hugetlb_bytes"] != nil {
		t.Fatal("missing NUMA file emitted residency")
	}
}
