package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	EventsIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nephtys_events_ingested_total",
		Help: "The total number of events ingested",
	}, []string{"stream_id"})

	EventsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nephtys_events_dropped_by_pipeline_total",
		Help: "The total number of events dropped by the pipeline",
	}, []string{"stream_id", "middleware"})

	BytesIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nephtys_bytes_ingested_total",
		Help: "The total number of bytes ingested",
	}, []string{"stream_id"})

	BytesPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nephtys_bytes_published_total",
		Help: "The total number of bytes published to NATS",
	}, []string{"stream_id"})

	EventsPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nephtys_events_published_total",
		Help: "The total number of events published to NATS",
	}, []string{"stream_id"})

	// StreamState is a one-hot gauge for the current operational state of each
	// registered stream. Exactly one of connected, reconnecting, errored, or
	// stopped is set to 1 for a stream; the other states are set to 0.
	StreamState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nephtys_stream_state",
		Help: "Current operational state of a Nephtys stream (one-hot).",
	}, []string{"stream_id", "state"})

	// EventProcessingDuration measures wall-clock time from ingestion (entering
	// the instrumented publish path) to the final NATS publish call returning,
	// in seconds. Buckets cover sub-millisecond hot paths through ~1s tail
	// latencies, which spans both market-data and sensor-class profiles.
	EventProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nephtys_event_processing_duration_seconds",
		Help:    "Wall-clock latency from ingestion to NATS publish completion.",
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"stream_id"})

	// DedupCacheSize tracks the current number of entries held by each
	// stream's dedup LRU. Compare against the per-stream cache_size config to
	// detect saturation (gauge near capacity means the dedup window is
	// effectively shorter than configured TTL).
	DedupCacheSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nephtys_dedup_cache_size",
		Help: "Current number of entries in each stream's dedup LRU.",
	}, []string{"stream_id"})

	// DedupCacheCapacity reports the configured LRU capacity for each stream's
	// dedup middleware (cfg.CacheSize, or the 1000 default). Exposing it as a
	// gauge lets a dashboard plot saturation as size/capacity without knowing
	// the stream config; it is set once when the middleware is built.
	DedupCacheCapacity = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nephtys_dedup_cache_capacity",
		Help: "Configured maximum number of entries in each stream's dedup LRU.",
	}, []string{"stream_id"})

	// DedupCacheEvictions counts LRU evictions caused by a full cache
	// (excludes TTL-expired entries replaced in-place). A growing rate
	// indicates the dedup cache is undersized for the unique-payload rate.
	DedupCacheEvictions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nephtys_dedup_cache_evictions_total",
		Help: "Total entries evicted from the dedup LRU due to capacity (not TTL).",
	}, []string{"stream_id"})
)

var streamStates = [...]string{"connected", "reconnecting", "errored", "stopped"}

// SetStreamState updates the one-hot state gauge for a stream.
func SetStreamState(streamID, current string) {
	for _, state := range streamStates {
		value := 0.0
		if state == current {
			value = 1
		}
		StreamState.WithLabelValues(streamID, state).Set(value)
	}
}

// DeleteStreamState removes all state series for an unregistered stream.
func DeleteStreamState(streamID string) {
	for _, state := range streamStates {
		StreamState.DeleteLabelValues(streamID, state)
	}
}
