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

// launcherPodLabel selects KubeVirt launcher pods (pods running a VM's
// QEMU/libvirt processes).
const launcherPodLabel = "kubevirt.io=virt-launcher"

// vmiNameLabel carries the VirtualMachineInstance name on launcher pods.
const vmiNameLabel = "vmi.kubevirt.io/id"

// PodLister lists KubeVirt launcher pods cluster-wide; the collector filters
// by node.
type PodLister struct {
	Client kubernetes.Interface
}

// NewInClusterPodLister builds a lister from the in-cluster service-account
// configuration.
func NewInClusterPodLister() (*PodLister, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &PodLister{Client: client}, nil
}

// LauncherPods implements exporter.PodLister.
func (p *PodLister) LauncherPods() ([]exporter.Pod, error) {
	pods, err := p.Client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{
		LabelSelector: launcherPodLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("listing launcher pods: %w", err)
	}
	out := make([]exporter.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		out = append(out, exporter.Pod{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       string(pod.UID),
			QOSClass:  string(pod.Status.QOSClass),
			VMIName:   pod.Labels[vmiNameLabel],
			NodeName:  pod.Spec.NodeName,
		})
	}
	return out, nil
}

var _ exporter.PodLister = (*PodLister)(nil)

// QOS check keeps the corev1 import used even if the label constants change.
var _ = corev1.PodQOSGuaranteed
