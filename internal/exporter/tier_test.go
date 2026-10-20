package exporter

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestCollectTiersMemoryBearingNodes(t *testing.T) {
	for _, hasMemory := range []bool{true, false} {
		for _, memoryBearing := range []bool{true, false} {
			t.Run(fmt.Sprintf("has_memory=%t/memory_bearing=%t", hasMemory, memoryBearing), func(t *testing.T) {
				root := t.TempDir()
				putFile(t, root, "devices/system/node/node0/meminfo", "Node 0 MemTotal: 1024 kB\n")
				total := 0
				if memoryBearing {
					total = 1024
				}
				putFile(t, root, "devices/system/node/node1/meminfo", fmt.Sprintf("Node 1 MemTotal: %d kB\n", total))
				if hasMemory {
					list := "0\n"
					if memoryBearing {
						list = "0-1\n"
					}
					putFile(t, root, "devices/system/node/has_memory", list)
				}
				c := newTestCollector(nil)
				c.opts.SysfsRoot = root
				ch := make(chan prometheus.Metric, 32)
				err := c.collectTiers(ch, TierMap{0: 4}, map[int]int{0: 0})
				close(ch)
				if memoryBearing {
					if err == nil || !strings.Contains(err.Error(), "NUMA node 1 missing from memory-tier interface") {
						t.Fatalf("missing memory-bearing node: %v", err)
					}
					if len(ch) != 0 {
						t.Fatal("partial topology emitted metrics")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(ch) != 7 {
					t.Fatalf("got %d metrics, want only node 0's seven metrics", len(ch))
				}
				for metric := range ch {
					var m dto.Metric
					if err := metric.Write(&m); err != nil {
						t.Fatal(err)
					}
					for _, label := range m.Label {
						if label.GetName() == "numa_node" && label.GetValue() != "0" {
							t.Fatalf("memoryless node emitted: %v", &m)
						}
					}
				}
			})
		}
	}
}

func TestListNUMANodesHasMemory(t *testing.T) {
	root := t.TempDir()
	// has_memory is authoritative, even if a CPU-only node has no meminfo.
	putFile(t, root, "devices/system/node/node1/placeholder", "")
	putFile(t, root, "devices/system/node/has_memory", "\n")
	if nodes, err := listNUMANodes(root); err != nil || len(nodes) != 0 {
		t.Fatalf("empty has_memory: nodes=%v, err=%v", nodes, err)
	}
	putFile(t, root, "devices/system/node/has_memory", "invalid")
	if _, err := listNUMANodes(root); err == nil {
		t.Fatal("invalid has_memory accepted")
	}
}
