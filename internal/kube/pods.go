package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/shanemcd/memory-tier-exporter/internal/exporter"
)

// DefaultPodLabelSelector is the default pod-discovery setting: an empty
// selector attributes every pod on the node. Restrict it (for example to
// kubevirt.io=virt-launcher) to attribute only a subset of pods.
const DefaultPodLabelSelector = ""

// PodLister lists the pods whose cgroups are attributed per workload.
// LabelSelector selects the pods (empty: all pods); NodeName, when set,
// restricts the listing to that node server-side.
type PodLister struct {
	Client        kubernetes.Interface
	LabelSelector string
	NodeName      string
}

// NewInClusterPodLister builds a lister from the in-cluster service-account
// configuration.
func NewInClusterPodLister(labelSelector, nodeName string) (*PodLister, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &PodLister{Client: client, LabelSelector: labelSelector, NodeName: nodeName}, nil
}

// Pods implements exporter.PodLister.
func (p *PodLister) Pods() ([]exporter.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: p.LabelSelector}
	if p.NodeName != "" {
		opts.FieldSelector = "spec.nodeName=" + p.NodeName
	}
	pods, err := p.Client.CoreV1().Pods("").List(context.Background(), opts)
	if err != nil {
		return nil, fmt.Errorf("listing workload pods: %w", err)
	}
	out := make([]exporter.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" {
			continue
		}
		ep := exporter.Pod{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       string(pod.UID),
			QOSClass:  string(pod.Status.QOSClass),
			NodeName:  pod.Spec.NodeName,
		}
		if owner := controllingOwner(pod); owner != nil {
			ep.OwnerKind, ep.OwnerName = owner.Kind, owner.Name
		}
		out = append(out, ep)
	}
	return out, nil
}

// controllingOwner returns the pod's controlling ownerReference, if any.
// Every well-behaved controller (a KubeVirt VirtualMachineInstance, a
// StatefulSet, DaemonSet, ReplicaSet, Job, ...) sets one; it is what gives
// the workload a stable identity that survives pod restarts and, for VMs,
// spans both launcher pods during a live migration.
func controllingOwner(pod *corev1.Pod) *metav1.OwnerReference {
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller {
			return ref
		}
	}
	return nil
}

var _ exporter.PodLister = (*PodLister)(nil)
