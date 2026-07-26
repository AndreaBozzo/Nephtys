package telemetry

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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

// TestExposedMetricNames pins the /metrics surface: every series Nephtys owns
// carries the nephtys_ prefix, and the set is exactly what we intend to expose.
// Metric names are a public contract for scrape configs and dashboards, so a
// rename should have to be deliberate enough to update this list.
func TestExposedMetricNames(t *testing.T) {
	want := []string{
		"nephtys_bytes_ingested_total",
		"nephtys_bytes_published_total",
		"nephtys_dedup_cache_capacity",
		"nephtys_dedup_cache_evictions_total",
		"nephtys_dedup_cache_size",
		"nephtys_event_processing_duration_seconds",
		"nephtys_events_dropped_by_pipeline_total",
		"nephtys_events_ingested_total",
		"nephtys_events_published_total",
		"nephtys_stream_state",
	}
	// Compared as a sorted list, so the literal above does not double as an
	// ordering constraint on whoever adds the next metric.
	sort.Strings(want)

	// A *Vec reports no metric family until it has at least one child, so
	// materialize one series per metric before gathering, and drop them again
	// afterwards so the default registry is left as this test found it.
	streamID := "metrics-test-names"
	t.Cleanup(func() {
		EventsIngested.DeleteLabelValues(streamID)
		EventsDropped.DeleteLabelValues(streamID, "filter")
		BytesIngested.DeleteLabelValues(streamID)
		BytesPublished.DeleteLabelValues(streamID)
		EventsPublished.DeleteLabelValues(streamID)
		EventProcessingDuration.DeleteLabelValues(streamID)
		DedupCacheSize.DeleteLabelValues(streamID)
		DedupCacheCapacity.DeleteLabelValues(streamID)
		DedupCacheEvictions.DeleteLabelValues(streamID)
		DeleteStreamState(streamID)
	})
	EventsIngested.WithLabelValues(streamID).Inc()
	EventsDropped.WithLabelValues(streamID, "filter").Inc()
	BytesIngested.WithLabelValues(streamID).Add(1)
	BytesPublished.WithLabelValues(streamID).Add(1)
	EventsPublished.WithLabelValues(streamID).Inc()
	EventProcessingDuration.WithLabelValues(streamID).Observe(0.001)
	DedupCacheSize.WithLabelValues(streamID).Set(1)
	DedupCacheCapacity.WithLabelValues(streamID).Set(1000)
	DedupCacheEvictions.WithLabelValues(streamID).Inc()
	SetStreamState(streamID, "connected")

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var got []string
	for _, f := range families {
		name := f.GetName()
		// The Go runtime and process collectors keep their standard names.
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") || strings.HasPrefix(name, "promhttp_") {
			continue
		}
		got = append(got, name)
	}
	sort.Strings(got)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("exposed Nephtys metrics:\n got %v\nwant %v", got, want)
	}
}

// seriesForStream returns every gathered series carrying the given stream_id,
// as "metric_name{extra_label=value}" strings.
func seriesForStream(t *testing.T, streamID string) []string {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var found []string
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "stream_id" && l.GetValue() == streamID {
					found = append(found, f.GetName())
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

// TestDeleteStreamSeriesRemovesEverything guards against unregistered streams
// leaving series behind: every registration would otherwise add permanent
// cardinality, and dashboards would keep charting a stream that is gone.
func TestDeleteStreamSeries(t *testing.T) {
	streamID := "metrics-test-delete-all"

	EventsIngested.WithLabelValues(streamID).Inc()
	EventsPublished.WithLabelValues(streamID).Inc()
	BytesIngested.WithLabelValues(streamID).Add(1)
	BytesPublished.WithLabelValues(streamID).Add(1)
	EventProcessingDuration.WithLabelValues(streamID).Observe(0.001)
	DedupCacheSize.WithLabelValues(streamID).Set(1)
	DedupCacheCapacity.WithLabelValues(streamID).Set(1000)
	DedupCacheEvictions.WithLabelValues(streamID).Inc()
	SetStreamState(streamID, "connected")
	// Two middleware label values, to prove the drop counter is cleared across
	// all of them rather than one well-known name.
	EventsDropped.WithLabelValues(streamID, "filter").Inc()
	EventsDropped.WithLabelValues(streamID, "dedup").Inc()

	if got := seriesForStream(t, streamID); len(got) == 0 {
		t.Fatal("no series materialized, test would pass vacuously")
	}

	DeleteStreamSeries(streamID)

	if got := seriesForStream(t, streamID); len(got) != 0 {
		t.Errorf("series left behind after DeleteStreamSeries: %v", got)
	}
}

// TestDeleteDedupSeries covers the narrower case of dedup being removed from a
// running stream's pipeline: the cache gauges must not keep reporting an
// occupancy and capacity that no longer describe anything.
func TestDeleteDedupSeries(t *testing.T) {
	streamID := "metrics-test-delete-dedup"
	t.Cleanup(func() { DeleteStreamSeries(streamID) })

	EventsIngested.WithLabelValues(streamID).Inc()
	DedupCacheSize.WithLabelValues(streamID).Set(5)
	DedupCacheCapacity.WithLabelValues(streamID).Set(50)
	DedupCacheEvictions.WithLabelValues(streamID).Inc()

	DeleteDedupSeries(streamID)

	got := seriesForStream(t, streamID)
	for _, name := range got {
		if strings.Contains(name, "dedup") {
			t.Errorf("dedup series survived DeleteDedupSeries: %v", got)
		}
	}
	// The stream is still registered, so its other series must be untouched.
	if len(got) != 1 || got[0] != "nephtys_events_ingested_total" {
		t.Errorf("non-dedup series should be untouched, got %v", got)
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
