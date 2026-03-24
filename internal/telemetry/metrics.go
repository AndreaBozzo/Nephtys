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
)
