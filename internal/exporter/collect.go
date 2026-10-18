package exporter

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// collect runs one full scrape. Individual source failures mark the scrape
// failed but partial metrics may still have been emitted.
func (c *Collector) collect(ch chan<- prometheus.Metric) error {
	tm, err := ReadTierMap(c.opts.SysfsRoot)
	if err != nil {
		return err
	}
	ranks := tm.Ranks()

	if err := c.collectTiers(ch, tm, ranks); err != nil {
		return err
	}
	if err := c.collectMovement(ch); err != nil {
		return err
	}
	if err := c.collectPSI(ch); err != nil {
		return err
	}
	// VM attribution failures do not fail the whole scrape: node-level tier
	// metrics are still valid, and a Kubernetes API hiccup should not blank
	// the node's tier visibility.
	if err := c.collectVMs(ch, tm, ranks); err != nil {
		ch <- prometheus.MustNewConstMetric(vmAttributionErrorsDesc, prometheus.CounterValue, 1, c.opts.NodeName)
	}
	return nil
}

var vmAttributionErrorsDesc = prometheus.NewDesc(
	"node_memory_tier_vm_attribution_error",
	"1 if the last per-VM attribution pass failed (node-level tier metrics remain valid).",
	[]string{"node"}, nil,
)

// Timestamp is used by tests to bound collection time.
var now = time.Now
