# memory-tier-exporter

A Prometheus exporter for Linux memory-tier state: which NUMA nodes hold DRAM
versus slower tiers (such as CXL memory), how much memory each tier carries,
how fast pages are moving between tiers, memory pressure, and — on Kubernetes
clusters — ordinary memcg residency and, where exposed by the kernel,
separate HugeTLB residency per pod and tier (workloads are
identified by their controlling owner, so KubeVirt VMs, StatefulSets,
DaemonSets and plain pods all attribute with no configuration).

## Why

Linux already tiers memory: CXL memory onboards as a NUMA node, the kernel
assigns it a memory tier, and under pressure reclaim demotes cold pages from
DRAM to the slower tier (and promotes hot pages back). Node-level collectors
expose none of this with default settings: no per-NUMA-node memory, no
demotion counters, no pressure. If you run VMs or other memory-hungry
workloads overcommitted onto CXL-extended hosts, you are flying blind about
where memory actually lives.

This exporter closes that gap with one small DaemonSet.

## Metrics

All metrics carry the `node_memory_tier_` prefix (no double-counting against
node_exporter) and a `node` label with the Kubernetes node name (the join key
with kubevirt and node metrics).

| Metric | Labels | Meaning |
|---|---|---|
| `node_memory_tier_nodes` | node, numa_node, tier, rank | Tier membership; `tier` is the kernel's memory-tier number (smaller = faster), `rank` is the zero-based position among tiers present (rank 0 = DRAM on current platforms) |
| `node_memory_tier_memtotal_bytes` / `memfree_bytes` / `memused_bytes` | node, numa_node, tier, rank | Per-NUMA-node memory (sysfs node meminfo) |
| `node_memory_tier_anon_bytes` / `file_bytes` | node, numa_node, tier, rank | Node AnonPages (includes anonymous THP, excludes explicit HugeTLB) vs FilePages (includes shmem/tmpfs and dirty/mapped pages, not necessarily immediately discardable) |
| `node_memory_tier_anon_thp_bytes` | node, numa_node, tier, rank | AnonHugePages breakdown, already included in anon_bytes; do not add them |
| `node_memory_tier_pgdemote_total` | node, mode | Cumulative demoted pages by reclaim path (kswapd/direct/khugepaged) |
| `node_memory_tier_pgpromote_total` | node, mode | Cumulative promoted pages by outcome |
| `node_memory_tier_psi_stalled_seconds_total` / `psi_stalled_ratio` | node, scope[, window] | Memory pressure stall information (when the kernel exposes PSI) |
| `node_memory_tier_workload_anon_bytes` / `workload_file_bytes` / `workload_shmem_bytes` | node, workload, workload_kind, namespace, pod, numa_node, tier, rank | Ordinary memcg memory by tier from the pod-parent `memory.numa_stat`; all containers, but excludes explicit HugeTLB guest RAM; shmem is a subset of file, not additive |
| `node_memory_tier_workload_pgdemote_total` / `workload_pgpromote_total` | node, workload, workload_kind, namespace, pod, mode, numa_node, tier, rank | Ordinary memcg page movement, not evidence of HugeTLB guest RAM movement |
| `node_memory_tier_workload_hugetlb_bytes` | node, workload, workload_kind, namespace, pod, numa_node, tier, rank, page_size | Separate explicit HugeTLB residency in bytes from `hugetlb.<size>.numa_stat`; page_size is e.g. `2MB` or `1GB` |
| `node_memory_tier_workload_hugetlb_numa_available` | node, workload, workload_kind, namespace, pod | 1 if at least one HugeTLB size is detected and every detected size has readable, valid NUMA stats; otherwise 0 |
| `node_memory_tier_workload_pods_discovered` / `workload_pods_read` | node | Selected local pods listed / pods with successfully read ordinary memory.numa_stat in this pass |
| `node_memory_tier_workload_pods_skipped` | node, reason | Skipped ordinary attribution, with bounded reasons `not_found`, `disappeared`, `read_error` |
| `node_memory_tier_workload_attribution_error` | node | Always a 0/1 gauge: 1 on API, topology, pod read/parse, or HugeTLB read/parse failure, including skipped pods; 0 on success or when disabled |
| `node_memory_tier_scrape_success` / `scrape_errors_total` | — | Node-source scrape success and cumulative failures, including the current scrape; workload errors have their own gauge |

On kernels without the memory-tier sysfs tree, every NUMA node is reported as
a single synthesized tier (rank 0), so DRAM-only clusters still get per-node
memory visibility. This fallback applies **only when the interface is absent**.
An existing but unreadable, malformed, empty, overlapping, or incomplete tier
map fails the node scrape and suppresses tier placement rather than inventing
rank 0. A workload NUMA node absent from an otherwise valid map is explicitly
labeled `tier="-1", rank="-1"` (unknown), never as fastest.

## Per-workload attribution

On Kubernetes, the exporter attributes per-workload memory by reading the
cgroup v2 `memory.numa_stat` of pods' cgroups. This is intentionally fully
generic — VMs are just pods:

- **Discovery** is a single flag, `--pod-label-selector` (default empty:
  every pod on the node). Restrict it, e.g.
  `--pod-label-selector=kubevirt.io=virt-launcher`, to attribute a subset.
- **Workload identity** is derived from each pod's *controlling owner*
  (`ownerReference` with `controller=true`): the `workload` label is the
  owner's name and `workload_kind` its kind (matching
  `kube-state-metrics`' `kube_pod_owner`), falling back to the pod's own
  name and `Pod` for ownerless pods.

Owner-based identity is what makes the series useful without any
KubeVirt-specific configuration:

- a KubeVirt VM's launcher pod is owned by its VirtualMachineInstance, so
  series carry `workload="<vmi-name>", workload_kind="VirtualMachineInstance"`
  — which joins directly against KubeVirt's own `kubevirt_vmi_*` metrics —
  and identity survives launcher-pod restarts. During a live migration both
  launcher pods share the VMI owner, so the VM's pages on the source and
  destination node aggregate under one workload.
- a StatefulSet's or DaemonSet's pods are owned by the StatefulSet/DaemonSet.
- a Deployment's pods are owned by their ReplicaSet, so `workload` is
  revision-scoped (it changes on each rollout); join through
  `kube_replicaset_owner` if you need the Deployment name.

### Accounting boundaries and coverage

The existing `workload_anon_bytes`, `workload_file_bytes`, and
`workload_shmem_bytes` names and identity labels describe **ordinary memcg
accounting**, not total VM guest RAM. They include all containers in the pod
(QEMU, libvirt and sidecars for launcher pods), but **omit explicit HugeTLB
guest RAM**. `workload_kind="VirtualMachineInstance"` identifies the owner,
not the scope of memory measured. Low ordinary-memcg CXL usage does not prove
that a VM's guest RAM is in DRAM. HugeTLB-backed guest RAM and launcher overhead
are separate; protecting guest RAM does not protect ordinary launcher memory.

`workload_hugetlb_bytes` reads only each pod-parent cgroup's
`hugetlb.<size>.numa_stat`, whose cgroup v2 format is a byte-valued line such as
`total=1073741824 N0=1073741824 N1=0`. Parent values include containers: the
exporter never recursively adds children or multiplies by page size. Kernel
interface availability varies; readable zero-valued NUMA stats are valid
coverage, not evidence of any HugeTLB allocation. The availability gauge is
emitted for every pod whose ordinary stats were read. It is 0 if no interface
is detected or any detected page size lacks valid NUMA stats. Valid sizes may
still emit residency when another size is unavailable. Kernels exposing only
`hugetlb.<size>.current` provide total bytes, **not placement**: no residency
series is fabricated, and availability is 0. Interface absence is not an
attribution error; malformed data or actual read errors are.

Both systemd and cgroupfs layouts support Guaranteed, Burstable and BestEffort
pods. Coverage counts refer to ordinary memcg reads (HugeTLB coverage is
separate). `not_found` means no supported pod directory was found, including a
pod that vanished before lookup; `disappeared` means its stats file vanished
after a directory was found; `read_error` includes other I/O or invalid data.
On listing failure or when disabled, counts are 0; on topology failure no
placement is attempted and counts are 0. Check health alongside counts.

Kubernetes listing has a **5-second deadline**, including request cancellation,
below the 30-second scrape budget. No background cache is used. Disable
attribution with `--no-workload-attribution` for a pure node-level exporter.

## Deployment (OpenShift)

```sh
podman build -t memory-tier-exporter:latest .
# push to your registry, then reference it in deploy/all.yaml
oc apply -f deploy/all.yaml
```

`deploy/all.yaml` creates a dedicated namespace (`cxl-observability`, labeled
for platform-monitoring selection), the DaemonSet (host mounts for /sys,
/proc, cgroupfs, read-only), RBAC (list pods for workload attribution), a
Service, and a ServiceMonitor. The exporter needs the `hostmount-anyuid` SCC on
OpenShift (granted by the manifest's cluster role binding step if you run it
manually: `oc adm policy add-scc-to-user hostmount-anyuid -z
memory-tier-exporter -n cxl-observability`).

On vanilla Kubernetes, point any Prometheus at the service; on clusters using
node_exporter's textfile collector instead, run the exporter with
`--no-workload-attribution` as a standalone HTTP endpoint.

### HTTP resource limits

Only one `/metrics` collection runs at a time; overlapping requests receive
HTTP 503. The server has 5-second header, 10-second read, 25-second write, and
60-second idle timeouts. A write timeout bounds network writes, not filesystem
collection; the concurrency slot stays occupied until collection finishes.
The endpoint is unauthenticated and includes workload identities: restrict
network access to trusted monitoring clients (NetworkPolicy/firewall or an
authenticated proxy as appropriate for your cluster).

## Recording rules and alerts

See the [cxl-memory-expansion design repository](https://github.com/shanemcd/cxl-memory-expansion)
for the recording rules (`node:memory_dram_used:ratio`,
`workload:memory_spilled:ratio`, ...), alerts, and the descheduler integration
that consumes them.

## Development

```sh
go test -race ./...
go vet ./...
go build ./cmd/memory-tier-exporter
```

Parsers are unit-tested against fixtures mirroring real kernel output
(`testdata/`). `cmd/memory-hog` is a bounded demonstration workload for
exercising demotion (allocate cold memory, keep a hot fraction touched).
