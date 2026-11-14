package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/metrics"
)

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
