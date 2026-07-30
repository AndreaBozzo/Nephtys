package pipeline

import (
	"context"

	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

// BuildFromConfig creates a pipeline populated with middlewares
// based on the per-stream configuration.
// The context controls the lifetime of stateful middlewares (e.g. batch worker).
//
// It fails on a configuration it cannot honour exactly as written rather than
// substituting defaults. Callers that accept configuration from outside the
// process should reject it with ValidateConfig first, so an operator gets a
// path-qualified rejection before any middleware is constructed; an error from
// here means a config passed validation and still could not be built.
func BuildFromConfig(ctx context.Context, streamID string, cfg *domain.PipelineConfig) (*Pipeline, error) {
	if cfg == nil {
		return New(), nil // Empty passthrough pipeline
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

	// 3. Dedup events before enrichment. NewDedup publishes the cache gauges
	// when it builds; when it does not build, clear any series left by a
	// previous pipeline so a hot-swap that drops dedup stops reporting stale
	// occupancy and capacity.
	dedup, err := NewDedup(streamID, cfg.Dedup)
	if err != nil {
		return nil, err
	}
	if dedup != nil {
		middlewares = append(middlewares, dedup)
	} else {
		telemetry.DeleteDedupSeries(streamID)
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
	batch, err := NewBatch(ctx, cfg.Batch)
	if err != nil {
		// Dedup already published its gauges by this point. Retract them, or a
		// pipeline that never ran would keep reporting occupancy and capacity.
		if dedup != nil {
			telemetry.DeleteDedupSeries(streamID)
		}
		return nil, err
	}
	if batch != nil {
		middlewares = append(middlewares, batch)
	}

	return New(middlewares...), nil
}
