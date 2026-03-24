package pipeline

import (
	"encoding/json"
	"log/slog"
	"math"
	"sync"

	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

// NewThreshold creates a middleware that drops numerical values if their
// absolute change is less than a configured delta.
func NewThreshold(streamID string, cfg *domain.ThresholdConfig) Middleware {
	if cfg == nil || !cfg.Enabled || cfg.Path == "" {
		return nil
	}

	var mu sync.Mutex
	lastValues := make(map[string]float64)

	return func(next Handler) Handler {
		return func(topic string, event domain.StreamEvent) error {
			if len(event.Payload) == 0 {
				return next(topic, event)
			}

			var original map[string]interface{}
			if err := json.Unmarshal(event.Payload, &original); err != nil {
				slog.Debug("Threshold: payload is not a JSON object, passing through", "source", event.Source)
				return next(topic, event)
			}

			valInterface, ok := extractValue(original, cfg.Path)
			if !ok {
				return next(topic, event) // Path not found
			}

			var currentVal float64
			switch v := valInterface.(type) {
			case float64:
				currentVal = v
			case float32:
				currentVal = float64(v)
			case int:
				currentVal = float64(v)
			default:
				// Not a numerical value, pass through
				return next(topic, event)
			}

			mu.Lock()
			lastVal, hasLast := lastValues[event.Source]

			if !hasLast || math.Abs(currentVal-lastVal) >= cfg.Delta {
				// Record new value and pass
				lastValues[event.Source] = currentVal
				mu.Unlock()
				return next(topic, event)
			}

			mu.Unlock()
			// Delta is too small, drop event
			telemetry.EventsDropped.WithLabelValues(streamID, "threshold").Inc()
			return nil
		}
	}
}
