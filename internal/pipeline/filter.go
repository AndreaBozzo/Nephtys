package pipeline

import (
	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

// NewFilter creates a middleware that drops events not matching the criteria.
func NewFilter(streamID string, cfg *domain.FilterConfig) Middleware {
	if cfg == nil || len(cfg.MatchTypes) == 0 {
		return nil // Passthrough
	}

	allowedTypes := make(map[string]bool)
	for _, t := range cfg.MatchTypes {
		allowedTypes[t] = true
	}

	return func(next Handler) Handler {
		return func(topic string, event domain.StreamEvent) error {
			if allowedTypes[event.Type] {
				return next(topic, event)
			}
			telemetry.EventsDropped.WithLabelValues(streamID, "filter").Inc()
			return nil
		}
	}
}
