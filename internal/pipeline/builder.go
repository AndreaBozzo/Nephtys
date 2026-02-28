package pipeline

import "nephtys/internal/domain"

// BuildFromConfig creates a pipeline populated with middlewares
// based on the per-stream configuration.
func BuildFromConfig(cfg *domain.PipelineConfig) *Pipeline {
	if cfg == nil {
		return New() // Empty passthrough pipeline
	}

	var middlewares []Middleware

	// 1. Filter out events early
	if filter := NewFilter(cfg.Filter); filter != nil {
		middlewares = append(middlewares, filter)
	}

	// 2. Transform payload shape
	if transform := NewTransform(cfg.Transform); transform != nil {
		middlewares = append(middlewares, transform)
	}

	// 3. Dedup events before enrichment
	if dedup := NewDedup(cfg.Dedup); dedup != nil {
		middlewares = append(middlewares, dedup)
	}

	// 3. Enrich remaining events
	if enrich := NewEnrich(cfg.Enrich); enrich != nil {
		middlewares = append(middlewares, enrich)
	}

	return New(middlewares...)
}
