package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	EventsIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "events_ingested_total",
		Help: "The total number of events ingested",
	}, []string{"stream_id"})

	EventsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "events_dropped_by_pipeline_total",
		Help: "The total number of events dropped by the pipeline",
	}, []string{"stream_id", "middleware"})

	BytesIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bytes_ingested_total",
		Help: "The total number of bytes ingested",
	}, []string{"stream_id"})

	BytesPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bytes_published_total",
		Help: "The total number of bytes published to NATS",
	}, []string{"stream_id"})

	EventsPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "events_published_total",
		Help: "The total number of events published to NATS",
	}, []string{"stream_id"})

	// EventProcessingDuration measures wall-clock time from ingestion (entering
	// the instrumented publish path) to the final NATS publish call returning,
	// in seconds. Buckets cover sub-millisecond hot paths through ~1s tail
	// latencies, which spans both market-data and sensor-class profiles.
	EventProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "event_processing_duration_seconds",
		Help:    "Wall-clock latency from ingestion to NATS publish completion.",
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"stream_id"})

	// DedupCacheSize tracks the current number of entries held by each
	// stream's dedup LRU. Compare against the per-stream cache_size config to
	// detect saturation (gauge near capacity means the dedup window is
	// effectively shorter than configured TTL).
	DedupCacheSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dedup_cache_size",
		Help: "Current number of entries in each stream's dedup LRU.",
	}, []string{"stream_id"})

	// DedupCacheEvictions counts LRU evictions caused by a full cache
	// (excludes TTL-expired entries replaced in-place). A growing rate
	// indicates the dedup cache is undersized for the unique-payload rate.
	DedupCacheEvictions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dedup_cache_evictions_total",
		Help: "Total entries evicted from the dedup LRU due to capacity (not TTL).",
	}, []string{"stream_id"})
)
