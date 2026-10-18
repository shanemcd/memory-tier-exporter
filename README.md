# memory-tier-exporter

A Prometheus exporter for Linux memory-tier state: which NUMA nodes hold DRAM
versus slower tiers (such as CXL memory), how much memory each tier carries,
how fast pages are moving between tiers, memory pressure, and — on Kubernetes
clusters running KubeVirt — how much of each VM's memory resides on each tier.

## Why

Linux already tiers memory: CXL memory onboards as a NUMA node, the kernel
assigns it a memory tier, and under pressure reclaim demotes cold pages from
DRAM to the slower tier (and promotes hot pages back). Node-level collectors
expose none of this with default settings: no per-NUMA-node memory, no
demotion counters, no pressure. If you run VMs overcommitted onto CXL-extended
hosts, you are flying blind about where memory actually lives.

This exporter closes that gap with one small DaemonSet.

## Metrics

All metrics carry the `node_memory_tier_` prefix (no double-counting against
node_exporter) and a `node` label with the Kubernetes node name (the join key
with kubevirt and node metrics).

| Metric | Labels | Meaning |
|---|---|---|
| `node_memory_tier_nodes` | node, numa_node, tier, rank | Tier membership; `tier` is the kernel's memory-tier number (smaller = faster), `rank` is the zero-based position among tiers present (rank 0 = DRAM on current platforms) |
| `node_memory_tier_memtotal_bytes` / `memfree_bytes` / `memused_bytes` | node, numa_node, tier, rank | Per-NUMA-node memory (sysfs node meminfo) |
| `node_memory_tier_anon_bytes` / `file_bytes` | node, numa_node, tier, rank | Anonymous vs file-backed memory per node; anon is what tiering moves and what a working set is made of |
| `node_memory_tier_pgdemote_total` | node, mode | Cumulative demoted pages by reclaim path (kswapd/direct/khugepaged) |
| `node_memory_tier_pgpromote_total` | node, mode | Cumulative promoted pages by outcome |
| `node_memory_tier_psi_stalled_seconds_total` / `psi_stalled_ratio` | node, scope[, window] | Memory pressure stall information (when the kernel exposes PSI) |
| `node_memory_tier_vm_anon_bytes` / `vm_file_bytes` / `vm_shmem_bytes` | node, vmi, namespace, numa_node, tier, rank | Per-VM memory by tier, from the launcher pod's cgroup `memory.numa_stat` (includes launcher overhead) |
| `node_memory_tier_vm_pgdemote_total` / `vm_pgpromote_total` | node, vmi, namespace, mode, numa_node, tier, rank | Per-VM page movement between tiers |

On kernels without the memory-tier sysfs tree, every NUMA node is reported as
a single synthesized tier (rank 0), so DRAM-only clusters still get per-node
memory visibility.

## Deployment (OpenShift)

```sh
podman build -t memory-tier-exporter:latest .
# push to your registry, then reference it in deploy/all.yaml
oc apply -f deploy/all.yaml
```

`deploy/all.yaml` creates a dedicated namespace (`cxl-observability`, labeled
for platform-monitoring selection), the DaemonSet (host mounts for /sys,
/proc, cgroupfs, read-only), RBAC (list pods for VM attribution), a Service,
and a ServiceMonitor. The exporter needs the `hostmount-anyuid` SCC on
OpenShift (granted by the manifest's cluster role binding step if you run it
manually: `oc adm policy add-scc-to-user hostmount-anyuid -z
memory-tier-exporter -n cxl-observability`).

On vanilla Kubernetes, point any Prometheus at the service; on clusters using
node_exporter's textfile collector instead, run the exporter with
`--no-vm-attribution` as a standalone HTTP endpoint.

## Recording rules and alerts

See the [cxl-memory-expansion design repository](https://github.com/shanemcd/cxl-memory-expansion)
for the recording rules (`node:memory_dram_used:ratio`,
`vmi:memory_spilled:ratio`, ...), alerts, and the descheduler integration that
consumes them.

## Development

```sh
go test ./...
go build ./cmd/memory-tier-exporter
```

Parsers are unit-tested against fixtures mirroring real kernel output
(`testdata/`). `cmd/memory-hog` is a bounded demonstration workload for
exercising demotion (allocate cold memory, keep a hot fraction touched).
