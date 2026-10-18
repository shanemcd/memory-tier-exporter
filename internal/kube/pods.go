package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/shanemcd/memory-tier-exporter/internal/exporter"
)

// Default pod-discovery settings implement the KubeVirt preset: attribute
// KubeVirt virtual machines by selecting their launcher pods and naming each
// workload with the VMI name carried on the pod's labels.
const (
	DefaultPodLabelSelector  = "kubevirt.io=virt-launcher"
	DefaultWorkloadNameLabel = "vmi.kubevirt.io/id"
	DefaultWorkloadKind      = "vmi"
)

// PodLister lists the pods backing attributed workloads. LabelSelector
// selects the pods; WorkloadNameLabel is the pod label whose value names the
// workload (empty: the pod name is used); NodeName, when set, restricts the
// listing to that node server-side.
type PodLister struct {
	Client            kubernetes.Interface
	LabelSelector     string
	WorkloadNameLabel string
	NodeName          string
}

// NewInClusterPodLister builds a lister from the in-cluster service-account
// configuration.
func NewInClusterPodLister(labelSelector, workloadNameLabel, nodeName string) (*PodLister, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &PodLister{
		Client:            client,
		LabelSelector:     labelSelector,
		WorkloadNameLabel: workloadNameLabel,
		NodeName:          nodeName,
	}, nil
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
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		out = append(out, exporter.Pod{
			Namespace:    pod.Namespace,
			Name:         pod.Name,
			UID:          string(pod.UID),
			QOSClass:     string(pod.Status.QOSClass),
			WorkloadName: pod.Labels[p.WorkloadNameLabel],
			NodeName:     pod.Spec.NodeName,
		})
	}
	return out, nil
}

var _ exporter.PodLister = (*PodLister)(nil)
