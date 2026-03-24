package pipeline

import (
	"strings"

	"nephtys/internal/domain"
)

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

// extractValue traverses a nested map using dot notation (e.g., "data.kline.c")
func extractValue(obj map[string]interface{}, path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	var current interface{} = obj

	for _, part := range parts {
		if currentMap, ok := current.(map[string]interface{}); ok {
			if val, exists := currentMap[part]; exists {
				current = val
			} else {
				return nil, false
			}
		} else {
			return nil, false
		}
	}

	return current, true
}

