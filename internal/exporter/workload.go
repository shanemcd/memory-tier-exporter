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

// workloadMetricLabels are the labels on per-workload tier-attribution
// metrics. workload, workload_kind, namespace and pod identify the attributed
// workload and the Kubernetes pod whose cgroup is measured. The pod cgroup
// includes every container in the pod, so for KubeVirt VMs the launcher
// overhead (QEMU, libvirt process, sidecars) is included in these numbers.
// Ordinary memcg rows exclude explicit HugeTLB guest RAM; owner labels
// identify the pod owner, not a complete guest-memory measurement.
var workloadMetricLabels = []string{"node", "workload", "workload_kind", "namespace", "pod", "numa_node", "tier", "rank"}

type workloadMemKind int

const (
	wlAnon workloadMemKind = iota
	wlFile
	wlShmem
)

func (k workloadMemKind) name() string {
	switch k {
	case wlAnon:
		return "anon"
	case wlFile:
		return "file"
	default:
		return "shmem"
	}
}

func workloadMemDesc(kind workloadMemKind) *prometheus.Desc {
	return prometheus.NewDesc(
		"node_memory_tier_workload_"+kind.name()+"_bytes",
		fmt.Sprintf("%s memory of the workload's pod cgroup on this NUMA node (ordinary cgroup v2 memory.numa_stat; includes all containers, excludes explicit HugeTLB guest RAM; shmem is a subset of file, not additive).", kind.name()),
		workloadMetricLabels, nil,
	)
}

var workloadPgDemoteDesc = prometheus.NewDesc(
	"node_memory_tier_workload_pgdemote_total",
	"Cumulative pages demoted from this workload's pod cgroup on this NUMA node (cgroup v2 memory.numa_stat pgdemote_* rows), by reclaim path.",
	append([]string{"node", "workload", "workload_kind", "namespace", "pod", "mode"}, "numa_node", "tier", "rank"),
	nil,
)

var workloadPgPromoteDesc = prometheus.NewDesc(
	"node_memory_tier_workload_pgpromote_total",
	"Cumulative pages promoted toward a faster tier from this workload's pod cgroup on this NUMA node (cgroup v2 memory.numa_stat pgpromote_* rows), by outcome.",
	append([]string{"node", "workload", "workload_kind", "namespace", "pod", "mode"}, "numa_node", "tier", "rank"),
	nil,
)

// NumaStat is a parsed cgroup v2 memory.numa_stat: row name -> NUMA node
// -> value. Memory rows use bytes; movement rows use page counts.
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
	stat := ParseNumaStat(string(data))
	for _, row := range []string{"anon", "file", "shmem"} {
		if len(stat[row]) == 0 {
			return nil, fmt.Errorf("memory.numa_stat missing valid %s row", row)
		}
	}
	return stat, nil
}

// podCgroupPath returns the cgroup v2 directory of a pod's cgroup, given the
// cgroupfs mount, the pod's UID and QOS class. It handles the systemd cgroup
// driver layout used by kubelet on cgroupfs v2 hosts:
//
//	/sys/fs/cgroup/kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<podUID escaped>.slice
//
// where dashes in the UID are escaped to underscores by systemd slice
// naming. The cgroupfs layout is also probed: kubepods/pod<uid> for
// Guaranteed, kubepods/{burstable,besteffort}/pod<uid> otherwise.
func podCgroupPath(cgroupfsRoot, uid, qosClass string) (string, error) {
	esc := strings.ReplaceAll(uid, "-", "_")
	qos := strings.ToLower(qosClass)
	candidates := []string{
		filepath.Join(cgroupfsRoot, "kubepods.slice", "kubepods-"+qos+".slice", "kubepods-"+qos+"-pod"+esc+".slice"),
		filepath.Join(cgroupfsRoot, "kubepods.slice", "kubepods-pod"+esc+".slice"),
		filepath.Join(cgroupfsRoot, "kubepods", qos, "pod"+uid),
		filepath.Join(cgroupfsRoot, "kubepods", "pod"+uid),
	}
	for _, p := range candidates {
		fi, err := os.Stat(p)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			continue
		}
		if fi.IsDir() {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// collectWorkloads attributes per-workload memory by NUMA node for
// pods on this node. Failures of individual pods are skipped (a pod that
// vanished between list and read, or a cgroup layout we do not
// understand). The workload identity is the pod's controlling owner —
// stable across pod restarts, and shared by both launcher pods of a
// live-migrating VM — falling back to the pod's own identity when the pod
// has no controlling owner.
func (c *Collector) collectWorkloads(ch chan<- prometheus.Metric, tm TierMap, ranks map[int]int) error {
	counts := workloadCounts{skipped: map[string]int{}}
	defer counts.emit(ch, c.opts.NodeName)
	if c.opts.PodLister == nil {
		return nil
	}
	pods, err := c.opts.PodLister.Pods()
	if err != nil {
		return err
	}
	var errs []error
	for _, pod := range pods {
		if pod.NodeName != "" && pod.NodeName != c.opts.NodeName {
			continue
		}
		counts.discovered++
		cg, err := podCgroupPath(c.opts.CgroupfsRoot, pod.UID, pod.QOSClass)
		if err != nil {
			reason := "read_error"
			if os.IsNotExist(err) {
				reason = "not_found"
			}
			counts.skipped[reason]++
			errs = append(errs, err)
			continue
		}
		stat, err := ReadNumaStat(cg)
		if err != nil {
			reason := "read_error"
			if os.IsNotExist(err) {
				reason = "disappeared"
			}
			counts.skipped[reason]++
			errs = append(errs, err)
			continue
		}
		counts.read++
		workload, kind := pod.Name, "Pod"
		if pod.OwnerName != "" {
			workload, kind = pod.OwnerName, pod.OwnerKind
		}
		identity := []string{c.opts.NodeName, workload, kind, pod.Namespace, pod.Name}
		if err := c.collectHugetlb(ch, cg, identity, tm, ranks); err != nil {
			errs = append(errs, err)
		}
		emit := func(row string, desc *prometheus.Desc) {
			nodes, ok := stat[row]
			if !ok {
				return
			}
			for n, v := range nodes {
				tier, rank := c.nodeTierInfo(tm, ranks, n)
				labels := []string{
					c.opts.NodeName, workload, kind, pod.Namespace, pod.Name,
					strconv.Itoa(n), strconv.Itoa(tier), strconv.Itoa(rank),
				}
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(v), labels...)
			}
		}
		emit("anon", workloadMemDesc(wlAnon))
		emit("file", workloadMemDesc(wlFile))
		emit("shmem", workloadMemDesc(wlShmem))

		// Per-workload movement rows ("pgdemote_kswapd N0=0 N1=0", "pgpromote_success N0=8", ...).
		for row, nodes := range stat {
			var mode string
			desc := workloadPgDemoteDesc
			switch {
			case strings.HasPrefix(row, "pgdemote_"):
				mode = strings.TrimPrefix(row, "pgdemote_")
			case strings.HasPrefix(row, "pgpromote_"):
				mode = strings.TrimPrefix(row, "pgpromote_")
				desc = workloadPgPromoteDesc
			default:
				continue
			}
			for n, v := range nodes {
				tier, rank := c.nodeTierInfo(tm, ranks, n)
				labels := []string{
					c.opts.NodeName, workload, kind, pod.Namespace, pod.Name, mode,
					strconv.Itoa(n), strconv.Itoa(tier), strconv.Itoa(rank),
				}
				ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), labels...)
			}
		}
	}
	return errors.Join(errs...)
}
