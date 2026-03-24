package pipeline

import "nephtys/internal/domain"

// Handler processes an event intended for a specific topic.
// It returns an error if the processing fails.
type Handler func(topic string, event domain.StreamEvent) error

// Middleware wraps a Handler.
type Middleware func(next Handler) Handler

// Pipeline chains zero or more middlewares.
type Pipeline struct {
	middlewares []Middleware
}

// New creates a new pipeline with the given middlewares.
func New(middlewares ...Middleware) *Pipeline {
	return &Pipeline{middlewares: middlewares}
}

// Execute wraps a final publish action with the middleware chain and
// returns a Handler that initiates the chain.
func (p *Pipeline) Execute(publish Handler) Handler {
	handler := publish
	for i := len(p.middlewares) - 1; i >= 0; i-- {
		handler = p.middlewares[i](handler)
	}
	return handler
}
