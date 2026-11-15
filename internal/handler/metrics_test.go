package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/metrics"
)

func TestSetPodSummaryUsesOnlyCurrentTargetPods(t *testing.T) {
	summary := map[string]float64{"pod_ready": 99}
	pods := []podMeta{
		{Name: "vta-current", Component: "vta", Ready: true},
		{Name: "dids-current", Component: "dids", Ready: true},
	}
	setPodSummary(summary, pods, metrics.Target{Scope: "component", Component: "vta"})
	if summary["pod_ready"] != 1 || summary["pod_total"] != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestMetricsStreamLimits(t *testing.T) {
	handler := NewMetricsHandler(nil, nil, nil, MetricsHandlerConfig{MaxStreamsPerUser: 2, MaxStreamsPerTarget: 1})
	target := metrics.Target{Scope: "component", Component: "vta"}
	releaseFirst, ok := handler.acquireStream(1, 42, target)
	if !ok {
		t.Fatal("first stream rejected")
	}
	if _, ok := handler.acquireStream(1, 42, target); ok {
		t.Fatal("duplicate target stream exceeded target limit")
	}
	releaseFirst()
	if releaseNext, ok := handler.acquireStream(1, 42, target); !ok {
		t.Fatal("released stream did not free capacity")
	} else {
		releaseNext()
	}
}
