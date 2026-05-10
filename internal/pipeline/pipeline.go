package pipeline

import (
	"strconv"
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

// extractValue traverses maps and arrays using dot notation (e.g.,
// "data.kline.c" or "0.sensordatavalues.1.value").
func extractValue(obj interface{}, path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	current := obj

	for _, part := range parts {
		switch currentValue := current.(type) {
		case map[string]interface{}:
			currentMap := currentValue
			if val, exists := currentMap[part]; exists {
				current = val
			} else {
				return nil, false
			}
		case []interface{}:
			idx, err := strconv.Atoi(part)
			if err != nil || idx < 0 || idx >= len(currentValue) {
				return nil, false
			}
			current = currentValue[idx]
		default:
			return nil, false
		}
	}

	return current, true
}
