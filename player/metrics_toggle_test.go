package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestMetricsArchitectureDefault(t *testing.T) {
	for _, arch := range []string{"mips", "mipsle", "mips64", "mips64le"} {
		if defaultMetrics(arch) {
			t.Fatalf("metrics enabled by default on %s", arch)
		}
	}
	for _, arch := range []string{"amd64", "arm", "arm64"} {
		if !defaultMetrics(arch) {
			t.Fatalf("metrics disabled by default on %s", arch)
		}
	}
}

func TestDisabledMetricsKeepBufferingAndDirectDial(t *testing.T) {
	before := metricsEnabled
	metricsEnabled = false
	t.Cleanup(func() { metricsEnabled = before })
	gauge := metricBufferBytes.WithLabelValues(audio(stageDownload, "slopify")...)
	read := func() float64 {
		var m dto.Metric
		if err := gauge.Write(&m); err != nil {
			t.Fatal(err)
		}
		return m.GetGauge().GetValue()
	}
	old := read()
	t.Cleanup(func() { gauge.Set(old) })
	gauge.Set(123)
	budget := newReadAheadBudget(64)
	if _, ok := budget.tryTake(32); !ok {
		t.Fatal("buffer reservation failed")
	}
	budget.take(8)
	budget.release(16)
	if budget.inUse() != 24 {
		t.Fatal("buffer accounting disabled with metrics")
	}
	if got := read(); got != 123 {
		t.Fatalf("disabled gauge changed: %v", got)
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	conn, err := newTCPConnSet().dial(&net.Dialer{})(context.Background(), "tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, tracked := conn.(*trackedConn); tracked {
		t.Fatal("metrics-disabled connection is still wrapped")
	}
}
