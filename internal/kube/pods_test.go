package kube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestPodListingDeadlineCancelsRequest(t *testing.T) {
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	lister := &PodLister{Client: client, NodeName: "node-test"}
	begin := time.Now()
	_, err = lister.podsWithTimeout(100 * time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listing error = %v, want deadline exceeded", err)
	}
	if time.Since(begin) > 2*time.Second {
		t.Fatal("pod listing exceeded its deadline")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe request cancellation")
	}
	if PodListTimeout <= 0 || PodListTimeout >= 30*time.Second {
		t.Fatal("default listing deadline must be below the scrape budget")
	}
}

func TestPodListingSelectorsAndOwner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fieldSelector") != "spec.nodeName=node-test" || r.URL.Query().Get("labelSelector") != "app=vm" {
			t.Error("missing listing selectors")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"launcher","namespace":"default","uid":"abc","annotations":{"kubernetes.io/config.mirror":"static-uid"},"ownerReferences":[{"kind":"VirtualMachineInstance","name":"vm","controller":true}]},"spec":{"nodeName":"node-test"},"status":{"qosClass":"Guaranteed"}},{"metadata":{"name":"pending"},"spec":{}},{"metadata":{"name":"completed"},"spec":{"nodeName":"node-test"},"status":{"phase":"Succeeded"}},{"metadata":{"name":"failed"},"spec":{"nodeName":"node-test"},"status":{"phase":"Failed"}}]}`))
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	lister := &PodLister{Client: client, NodeName: "node-test", LabelSelector: "app=vm"}
	pods, err := lister.Pods()
	if err != nil || len(pods) != 1 || pods[0].UID != "static-uid" || pods[0].OwnerName != "vm" || pods[0].OwnerKind != "VirtualMachineInstance" || pods[0].QOSClass != "Guaranteed" {
		t.Fatalf("pods = %v, %v", pods, err)
	}
}
