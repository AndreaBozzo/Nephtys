package pipeline

import (
	"nephtys/internal/domain"
)

// NewFilter creates a middleware that drops events not matching the criteria.
func NewFilter(cfg *domain.FilterConfig) Middleware {
	if cfg == nil || len(cfg.MatchTypes) == 0 {
		return nil // Passthrough
	}

	allowedTypes := make(map[string]bool)
	for _, t := range cfg.MatchTypes {
		allowedTypes[t] = true
	}

	return func(event domain.StreamEvent) (domain.StreamEvent, bool) {
		if allowedTypes[event.Type] {
			return event, true
		}
		// Drop event
		return event, false
	}
}
