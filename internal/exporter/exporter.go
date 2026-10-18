// Package exporter implements the memory-tier Prometheus collector.
//
// The collector reads Linux kernel memory-tier state from sysfs and procfs
// and exposes it as Prometheus metrics:
//
//   - the mapping from NUMA nodes to memory tiers
//     (/sys/devices/virtual/memory_tiering),
//   - per-NUMA-node memory usage (/sys/devices/system/node/nodeN/meminfo),
//   - page demotion and promotion counters (/proc/vmstat),
//   - memory pressure stall information (/proc/pressure/memory),
//   - and, when running in a Kubernetes cluster, per-workload memory
//     attribution by NUMA node (cgroup v2 memory.numa_stat of each
//     workload pod's cgroup). The default configuration attributes KubeVirt
//     virtual machines; any pod set can be attributed by configuring pod
//     discovery.
//
// All metrics use the node_memory_tier_ prefix so they cannot be confused
// with (or double-counted against) node_exporter's own collectors, none of
// which expose this information with default settings.
package exporter

import (
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Options configures the collector.
type Options struct {
	// NodeName identifies this machine on all exported series. Usually
	// injected via the Kubernetes downward API.
	NodeName string

	// SysfsRoot, ProcfsRoot and CgroupfsRoot locate the kernel filesystems.
	// They default to /sys, /proc and /sys/fs/cgroup; tests override them.
	SysfsRoot    string
	ProcfsRoot   string
	CgroupfsRoot string

	// PodLister, when non-nil, enables per-workload attribution: it returns
	// the pods backing attributed workloads.
	PodLister PodLister

	// WorkloadKind is the value of the workload_kind label on per-workload
	// series. It should describe what the configured PodLister selects (the
	// KubeVirt preset uses "vmi"). Empty defaults to "pod".
	WorkloadKind string
}

// Pod is the minimal view of a Kubernetes pod the collector needs.
type Pod struct {
	Namespace    string
	Name         string // pod name
	UID          string // pod UID, used to locate the cgroup
	QOSClass     string // Guaranteed | Burstable | BestEffort
	WorkloadName string // workload name, from the configured name label
	NodeName     string // node the pod is scheduled on
}

// PodLister returns the pods whose cgroups are attributed per workload.
// Implementations typically list pods by a label selector and filter by
// node; the KubeVirt preset lists kubevirt.io=virt-launcher pods and names
// workloads with the vmi.kubevirt.io/id label.
type PodLister interface {
	Pods() ([]Pod, error)
}

// Collector implements prometheus.Collector.
type Collector struct {
	opts Options

	scrapeDuration prometheus.Gauge
	scrapeSuccess  prometheus.Gauge
	scrapeErrors   prometheus.Counter
}

// NewCollector builds a collector.
func NewCollector(opts Options) *Collector {
	if opts.SysfsRoot == "" {
		opts.SysfsRoot = "/sys"
	}
	if opts.ProcfsRoot == "" {
		opts.ProcfsRoot = "/proc"
	}
	if opts.CgroupfsRoot == "" {
		opts.CgroupfsRoot = "/sys/fs/cgroup"
	}
	if opts.NodeName == "" {
		if hn, err := os.Hostname(); err == nil {
			opts.NodeName = hn
		}
	}
	return &Collector{
		opts: opts,
		scrapeDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "node_memory_tier",
			Name:      "scrape_duration_seconds",
			Help:      "Duration of the last memory-tier collector scrape.",
		}),
		scrapeSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "node_memory_tier",
			Name:      "scrape_success",
			Help:      "Whether the last memory-tier collector scrape succeeded (1) or failed (0).",
		}),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "node_memory_tier",
			Name:      "scrape_errors_total",
			Help:      "Total number of memory-tier collector scrape errors.",
		}),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	c.scrapeDuration.Describe(ch)
	c.scrapeSuccess.Describe(ch)
	c.scrapeErrors.Describe(ch)
	ch <- tierNodesDesc
	ch <- tierMemDesc(tierMemTotal)
	ch <- tierMemDesc(tierMemFree)
	ch <- tierMemDesc(tierMemUsed)
	ch <- tierAnonDesc
	ch <- tierAnonTHPDesc
	ch <- tierFileDesc
	ch <- pgDemoteDesc
	ch <- pgPromoteDesc
	ch <- psiStalledDesc
	ch <- psiAvgDesc
	ch <- workloadMemDesc(wlAnon)
	ch <- workloadMemDesc(wlFile)
	ch <- workloadMemDesc(wlShmem)
	ch <- workloadPgDemoteDesc
	ch <- workloadPgPromoteDesc
	ch <- workloadAttributionErrorsDesc
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	begin := time.Now()
	err := c.collect(ch)
	c.scrapeDuration.Set(time.Since(begin).Seconds())
	c.scrapeSuccess.Set(btoi(err == nil))
	c.scrapeDuration.Collect(ch)
	c.scrapeSuccess.Collect(ch)
	c.scrapeErrors.Collect(ch)
	if err != nil {
		c.scrapeErrors.Inc()
	}
}

func btoi(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
