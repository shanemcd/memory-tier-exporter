package exporter

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// collect runs independent sources even when one fails. Workload failures have
// their own health gauge and do not invalidate otherwise valid node metrics.
func (c *Collector) collect(ch chan<- prometheus.Metric) error {
	tm, topologyErr := ReadTierMap(c.opts.SysfsRoot)
	ranks := tm.Ranks()
	if topologyErr == nil {
		topologyErr = c.collectTiers(ch, tm, ranks)
	}
	movementErr := c.collectMovement(ch)
	psiErr := c.collectPSI(ch)
	var attributionErr error
	if topologyErr == nil {
		attributionErr = c.collectWorkloads(ch, tm, ranks)
	} else {
		// Placement would be misleading with incomplete topology.
		counts := workloadCounts{}
		counts.emit(ch, c.opts.NodeName)
		if c.opts.PodLister != nil {
			attributionErr = topologyErr
		}
	}
	ch <- prometheus.MustNewConstMetric(workloadAttributionErrorsDesc, prometheus.GaugeValue, btoi(attributionErr != nil), c.opts.NodeName)
	return errors.Join(topologyErr, movementErr, psiErr)
}

var workloadAttributionErrorsDesc = prometheus.NewDesc(
	"node_memory_tier_workload_attribution_error",
	"Whether the last workload attribution pass failed or skipped pods (1) or succeeded/was disabled (0); node metrics can remain valid. HugeTLB interface absence is reported by its availability gauge, not an error.",
	[]string{"node"}, nil,
)
