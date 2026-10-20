package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

type blockingGatherer struct {
	started chan struct{}
	release chan struct{}
}

func (g *blockingGatherer) Gather() ([]*dto.MetricFamily, error) {
	close(g.started)
	<-g.release
	return nil, nil
}

func TestMetricsConcurrencyLimit(t *testing.T) {
	g := &blockingGatherer{started: make(chan struct{}), release: make(chan struct{})}
	handler := metricsHandler(g)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}()
	select {
	case <-g.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first scrape never started")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	close(g.release)
	<-finished
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("concurrent scrape status = %d, want 503", response.Code)
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	server := newHTTPServer(":9102", http.NotFoundHandler())
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 10*time.Second || server.WriteTimeout != 25*time.Second || server.IdleTimeout != 60*time.Second {
		t.Fatalf("unexpected timeouts: %+v", server)
	}
}
