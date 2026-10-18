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
//   - and, when running in a Kubernetes cluster with KubeVirt, per-VM memory
//     attribution by NUMA node (cgroup v2 memory.numa_stat of each VM's
//     launcher pod).
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

	// PodLister, when non-nil, enables per-VM attribution: it returns the
	// launcher pods running on this node.
	PodLister PodLister
}

// Pod is the minimal view of a Kubernetes pod the collector needs.
type Pod struct {
	Namespace string
	Name      string // launcher pod name (virt-launcher-<vmi>-<suffix>)
	UID       string // pod UID, used to locate the cgroup
	QOSClass  string // Guaranteed | Burstable | BestEffort
	VMIName   string // VMI name, from the vmi.kubevirt.io/id label
	NodeName  string // node the pod is scheduled on
}

// PodLister returns launcher pods. Implementations typically list pods with
// the kubevirt.io=virt-launcher label and filter by node.
type PodLister interface {
	LauncherPods() ([]Pod, error)
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
	ch <- vmMemDesc(vmAnon)
	ch <- vmMemDesc(vmFile)
	ch <- vmMemDesc(vmShmem)
	ch <- vmPgDemoteDesc
	ch <- vmPgPromoteDesc
	ch <- vmAttributionErrorsDesc
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
