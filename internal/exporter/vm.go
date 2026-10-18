package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// vmMetricLabels are the labels on per-VM tier-attribution metrics.
// vmi and namespace identify the VirtualMachineInstance whose launcher pod's
// cgroup is being measured; the launcher's own overhead (QEMU, libvirt
// process, sidecars) is included in these numbers.
var vmMetricLabels = []string{"node", "vmi", "namespace", "numa_node", "tier", "rank"}

type vmMemKind int

const (
	vmAnon vmMemKind = iota
	vmFile
	vmShmem
)

func (k vmMemKind) name() string {
	switch k {
	case vmAnon:
		return "anon"
	case vmFile:
		return "file"
	default:
		return "shmem"
	}
}

func vmMemDesc(kind vmMemKind) *prometheus.Desc {
	return prometheus.NewDesc(
		"node_memory_tier_vm_"+kind.name()+"_bytes",
		fmt.Sprintf("%s memory of the VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat; includes launcher overhead).", kind.name()),
		vmMetricLabels, nil,
	)
}

var vmPgDemoteDesc = prometheus.NewDesc(
	"node_memory_tier_vm_pgdemote_total",
	"Cumulative pages demoted from this VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat pgdemote_* rows), by reclaim path.",
	append([]string{"node", "vmi", "namespace", "mode"}, "numa_node", "tier", "rank"),
	nil,
)

var vmPgPromoteDesc = prometheus.NewDesc(
	"node_memory_tier_vm_pgpromote_total",
	"Cumulative pages promoted toward a faster tier from this VM's launcher pod cgroup on this NUMA node (cgroup v2 memory.numa_stat pgpromote_* rows), by outcome.",
	append([]string{"node", "vmi", "namespace", "mode"}, "numa_node", "tier", "rank"),
	nil,
)

// NumaStat is a parsed cgroup v2 memory.numa_stat: row name -> NUMA node -> bytes.
type NumaStat map[string]map[int]int64

// ParseNumaStat parses cgroup v2 memory.numa_stat content, e.g.
// "anon N0=160829440 N1=0".
func ParseNumaStat(content string) NumaStat {
	out := NumaStat{}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		row := fields[0]
		if _, ok := out[row]; !ok {
			out[row] = map[int]int64{}
		}
		for _, x := range fields[1:] {
			parts := strings.SplitN(x, "=", 2)
			if len(parts) != 2 || !strings.HasPrefix(parts[0], "N") {
				continue
			}
			n, err := strconv.Atoi(strings.TrimPrefix(parts[0], "N"))
			if err != nil {
				continue
			}
			v, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				continue
			}
			out[row][n] = v
		}
	}
	return out
}

// ReadNumaStat reads memory.numa_stat from a cgroup directory.
func ReadNumaStat(cgroupDir string) (NumaStat, error) {
	data, err := os.ReadFile(filepath.Join(cgroupDir, "memory.numa_stat"))
	if err != nil {
		return nil, err
	}
	return ParseNumaStat(string(data)), nil
}

// podCgroupPath returns the cgroup v2 directory of a pod's cgroup, given the
// cgroupfs mount, the pod's UID and QOS class. It handles the systemd cgroup
// driver layout used by kubelet on cgroupfs v2 hosts:
//
//	/sys/fs/cgroup/kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<podUID escaped>.slice
//
// where dashes in the UID are escaped to underscores by systemd slice
// naming. The non-systemd layout (kubepods/pod<uid>) is also probed.
func podCgroupPath(cgroupfsRoot, uid, qosClass string) string {
	esc := strings.ReplaceAll(uid, "-", "_")
	qos := strings.ToLower(qosClass)
	candidates := []string{
		filepath.Join(cgroupfsRoot, "kubepods.slice", "kubepods-"+qos+".slice", "kubepods-"+qos+"-pod"+esc+".slice"),
		filepath.Join(cgroupfsRoot, "kubepods.slice", "kubepods-pod"+esc+".slice"),
		filepath.Join(cgroupfsRoot, "kubepods", "pod"+uid),
	}
	for _, p := range candidates {
		if fi, err := os.Stat(filepath.Join(p, "memory.numa_stat")); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// collectVMs attributes per-VM memory by NUMA node for launcher pods on this
// node. Failures of individual pods are skipped (a pod that vanished between
// list and read, or a cgroup layout we do not understand).
func (c *Collector) collectVMs(ch chan<- prometheus.Metric, tm TierMap, ranks map[int]int) error {
	if c.opts.PodLister == nil {
		return nil
	}
	pods, err := c.opts.PodLister.LauncherPods()
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if pod.NodeName != "" && pod.NodeName != c.opts.NodeName {
			continue
		}
		cg := podCgroupPath(c.opts.CgroupfsRoot, pod.UID, pod.QOSClass)
		if cg == "" {
			continue
		}
		stat, err := ReadNumaStat(cg)
		if err != nil {
			continue
		}
		vmiName := pod.VMIName
		if vmiName == "" {
			vmiName = strings.TrimPrefix(strings.TrimPrefix(pod.Name, "virt-launcher-"), "")
		}
		emit := func(row string, desc *prometheus.Desc) {
			nodes, ok := stat[row]
			if !ok {
				return
			}
			for n, v := range nodes {
				tier, rank := c.nodeTierInfo(tm, ranks, n)
				labels := []string{
					c.opts.NodeName, vmiName, pod.Namespace,
					strconv.Itoa(n), strconv.Itoa(tier), strconv.Itoa(rank),
				}
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(v), labels...)
			}
		}
		emit("anon", vmMemDesc(vmAnon))
		emit("file", vmMemDesc(vmFile))
		emit("shmem", vmMemDesc(vmShmem))

		// Per-VM movement rows ("pgdemote_kswapd N0=0 N1=0", "pgpromote_success N0=8", ...).
		for row, nodes := range stat {
			var mode string
			desc := vmPgDemoteDesc
			switch {
			case strings.HasPrefix(row, "pgdemote_"):
				mode = strings.TrimPrefix(row, "pgdemote_")
			case strings.HasPrefix(row, "pgpromote_"):
				mode = strings.TrimPrefix(row, "pgpromote_")
				desc = vmPgPromoteDesc
			default:
				continue
			}
			for n, v := range nodes {
				tier, rank := c.nodeTierInfo(tm, ranks, n)
				labels := []string{
					c.opts.NodeName, vmiName, pod.Namespace, mode,
					strconv.Itoa(n), strconv.Itoa(tier), strconv.Itoa(rank),
				}
				ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), labels...)
			}
		}
	}
	return nil
}
