// Package pipeline provides a middleware chain for processing stream events
// between ingestion and publishing. Currently a passthrough — extend by
// registering Middleware functions (filtering, enrichment, dedup, etc.).
package pipeline

import "nephtys/internal/domain"

// Middleware processes an event and returns the (possibly modified) event
// and a boolean indicating whether the event should continue down the chain.
// Returning false drops the event.
type Middleware func(event domain.StreamEvent) (domain.StreamEvent, bool)

// Pipeline chains zero or more middlewares.
type Pipeline struct {
	steps []Middleware
}

// New creates an empty pipeline (passthrough).
func New(middlewares ...Middleware) *Pipeline {
	return &Pipeline{steps: middlewares}
}

// Process runs the event through all registered middlewares in order.
// Returns the final event and whether it should be published.
func (p *Pipeline) Process(event domain.StreamEvent) (domain.StreamEvent, bool) {
	for _, step := range p.steps {
		var ok bool
		event, ok = step(event)
		if !ok {
			return event, false
		}
	}
	return event, true
}
