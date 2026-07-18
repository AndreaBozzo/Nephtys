package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsRegistered(t *testing.T) {
	// Verify each metric can be incremented without panics
	// and that label cardinality is correct.

	tests := []struct {
		name   string
		inc    func()
		metric string
	}{
		{
			name: "EventsIngested",
			inc:  func() { EventsIngested.WithLabelValues("test-stream").Inc() },
		},
		{
			name: "EventsDropped",
			inc:  func() { EventsDropped.WithLabelValues("test-stream", "filter").Inc() },
		},
		{
			name: "BytesIngested",
			inc:  func() { BytesIngested.WithLabelValues("test-stream").Add(1024) },
		},
		{
			name: "BytesPublished",
			inc:  func() { BytesPublished.WithLabelValues("test-stream").Add(512) },
		},
		{
			name: "EventsPublished",
			inc:  func() { EventsPublished.WithLabelValues("test-stream").Inc() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic
			tt.inc()
		})
	}
}

func TestEventsIngestedCounter(t *testing.T) {
	// Use a unique label to avoid interference from other tests
	label := "metrics-test-ingested"

	EventsIngested.WithLabelValues(label).Inc()
	EventsIngested.WithLabelValues(label).Inc()

	val := testutil.ToFloat64(EventsIngested.WithLabelValues(label))
	if val != 2 {
		t.Errorf("expected 2, got %f", val)
	}
}

func TestEventsDroppedCounter(t *testing.T) {
	label := "metrics-test-dropped"

	EventsDropped.WithLabelValues(label, "threshold").Inc()

	val := testutil.ToFloat64(EventsDropped.WithLabelValues(label, "threshold"))
	if val != 1 {
		t.Errorf("expected 1, got %f", val)
	}
}

func TestStreamStateGaugeIsOneHot(t *testing.T) {
	streamID := "metrics-test-state"
	t.Cleanup(func() { DeleteStreamState(streamID) })

	SetStreamState(streamID, "reconnecting")
	if got := testutil.ToFloat64(StreamState.WithLabelValues(streamID, "reconnecting")); got != 1 {
		t.Fatalf("reconnecting gauge = %v, want 1", got)
	}

	SetStreamState(streamID, "connected")
	if got := testutil.ToFloat64(StreamState.WithLabelValues(streamID, "connected")); got != 1 {
		t.Errorf("connected gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(StreamState.WithLabelValues(streamID, "reconnecting")); got != 0 {
		t.Errorf("stale reconnecting gauge = %v, want 0", got)
	}
}
