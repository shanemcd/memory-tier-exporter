package exporter

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// movementDesc describes cumulative page movement counters from /proc/vmstat.
// mode is the reclaim path for demotion (kswapd, direct, khugepaged) or the
// outcome for promotion (success, candidate, ...).
func movementDesc(prefix string) *prometheus.Desc {
	help := "Cumulative pages demoted from a faster to a slower memory tier, by reclaim path (from /proc/vmstat pgdemote_*)."
	if prefix == "pgpromote" {
		help = "Cumulative pages promoted toward a faster memory tier, by outcome (from /proc/vmstat pgpromote_*)."
	}
	return prometheus.NewDesc(
		"node_memory_tier_"+prefix+"_total",
		help,
		[]string{"node", "mode"}, nil,
	)
}

var (
	pgDemoteDesc = movementDesc("pgdemote")
	pgPromoteDesc = movementDesc("pgpromote")
)

// ReadVMstatField returns one integer field from a procfs vmstat file.
func ReadVMstatField(procfsRoot, key string) (int64, bool) {
	f, err := os.Open(filepath.Join(procfsRoot, "vmstat"))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == key {
			v, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

// collectMovement emits pgdemote_*/pgpromote_* counters from /proc/vmstat.
// Unknown keys are skipped; kernels without tiering simply report nothing.
func (c *Collector) collectMovement(ch chan<- prometheus.Metric) error {
	type key struct{ prefix, mode string }
	values := map[key]int64{}

	f, err := os.Open(filepath.Join(c.opts.ProcfsRoot, "vmstat"))
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		var prefix string
		switch {
		case strings.HasPrefix(fields[0], "pgdemote_"):
			prefix = "pgdemote"
		case strings.HasPrefix(fields[0], "pgpromote_"):
			prefix = "pgpromote"
		default:
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		values[key{prefix, strings.TrimPrefix(strings.TrimPrefix(fields[0], prefix), "_")}] = v
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for k, v := range values {
		desc := pgDemoteDesc
		if k.prefix == "pgpromote" {
			desc = pgPromoteDesc
		}
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), c.opts.NodeName, k.mode)
	}
	return nil
}
