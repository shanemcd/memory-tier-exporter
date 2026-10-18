// memory-tier-exporter exposes Linux memory-tier state (DRAM vs CXL vs swap
// tiers, page demotion and promotion, memory pressure, and per-workload
// tier attribution) as Prometheus metrics.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/shanemcd/memory-tier-exporter/internal/exporter"
	"github.com/shanemcd/memory-tier-exporter/internal/kube"
)

func main() {
	var (
		listenAddress = flag.String("web.listen-address", ":9102",
			"Address on which to expose metrics.")
		metricsPath = flag.String("web.telemetry-path", "/metrics",
			"Path under which to expose metrics.")
		nodeName = flag.String("node-name", envOr("NODE_NAME", ""),
			"Kubernetes node name to attach to all series (defaults to $NODE_NAME, then hostname).")
		podLabelSelector = flag.String("pod-label-selector", kube.DefaultPodLabelSelector,
			"Label selector choosing the pods whose cgroups are attributed per workload (empty: all pods).")
		sysfsRoot = flag.String("sysfs.root", "/sys",
			"Sysfs mount point (testing).")
		procfsRoot = flag.String("procfs.root", "/proc",
			"Procfs mount point (testing).")
		cgroupfsRoot = flag.String("cgroupfs.root", "/sys/fs/cgroup",
			"Cgroup v2 mount point (testing).")
		disableWorkload = flag.Bool("no-workload-attribution", false,
			"Disable per-workload attribution (node-level tier metrics only).")
	)
	flag.Parse()

	node := *nodeName
	if node == "" {
		if hn, err := os.Hostname(); err == nil {
			node = hn
		}
	}

	var podLister exporter.PodLister
	if !*disableWorkload {
		pl, err := kube.NewInClusterPodLister(*podLabelSelector, node)
		if err != nil {
			log.Printf("workload attribution disabled (no in-cluster Kubernetes configuration: %v)", err)
		} else {
			podLister = pl
		}
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		exporter.NewCollector(exporter.Options{
			NodeName:     node,
			SysfsRoot:    *sysfsRoot,
			ProcfsRoot:   *procfsRoot,
			CgroupfsRoot: *cgroupfsRoot,
			PodLister:    podLister,
		}),
		versioncollector.NewCollector("memory_tier_exporter"),
	)

	http.Handle(*metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><head><title>memory-tier-exporter</title></head>
<body><h1>memory-tier-exporter</h1><p><a href="%s">Metrics</a></p></body></html>`,
			strings.TrimPrefix(*metricsPath, "/"))
	})

	log.Printf("starting memory-tier-exporter on %s (node %q)", *listenAddress, node)
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
