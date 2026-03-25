package pipeline

import (
	"context"

	"nephtys/internal/domain"
)

// BuildFromConfig creates a pipeline populated with middlewares
// based on the per-stream configuration.
// The context controls the lifetime of stateful middlewares (e.g. batch worker).
func BuildFromConfig(ctx context.Context, streamID string, cfg *domain.PipelineConfig) *Pipeline {
	if cfg == nil {
		return New() // Empty passthrough pipeline
	}

	var middlewares []Middleware

	// 1. Filter out events early
	if filter := NewFilter(streamID, cfg.Filter); filter != nil {
		middlewares = append(middlewares, filter)
	}

	// 2. Transform payload shape
	if transform := NewTransform(cfg.Transform); transform != nil {
		middlewares = append(middlewares, transform)
	}

	// 3. Dedup events before enrichment
	if dedup := NewDedup(streamID, cfg.Dedup); dedup != nil {
		middlewares = append(middlewares, dedup)
	}

	// 4. Enrich remaining events
	if enrich := NewEnrich(cfg.Enrich); enrich != nil {
		middlewares = append(middlewares, enrich)
	}

	// 5. Threshold/Delta Filtering
	if threshold := NewThreshold(streamID, cfg.Threshold); threshold != nil {
		middlewares = append(middlewares, threshold)
	}

	// 6. Batching (always output as array if enabled, so it's typically the last step)
	if batch := NewBatch(ctx, cfg.Batch); batch != nil {
		middlewares = append(middlewares, batch)
	}

	return New(middlewares...)
}
