package pipeline

import (
	"encoding/json"
	"log/slog"
	"nephtys/internal/domain"
)

// NewEnrich creates a middleware that injects tags into the event payload.
// If the payload is a JSON object, tags are added at the root.
func NewEnrich(cfg *domain.EnrichConfig) Middleware {
	if cfg == nil || len(cfg.Tags) == 0 {
		return nil
	}

	return func(next Handler) Handler {
		return func(topic string, event domain.StreamEvent) error {
			if len(event.Payload) == 0 {
				return next(topic, event)
			}

			// Try to parse payload as JSON object
			var obj map[string]interface{}
			if err := json.Unmarshal(event.Payload, &obj); err != nil {
				// Payload is not a JSON object, just pass it through
				slog.Debug("Enrich: payload is not a JSON object, skipping", "source", event.Source)
				return next(topic, event)
			}

			// Inject tags
			for k, v := range cfg.Tags {
				obj[k] = v
			}

			// Repack payload
			newPayload, err := json.Marshal(obj)
			if err != nil {
				slog.Error("Enrich: failed to marshal enriched payload", "error", err)
				return next(topic, event) // Pass original on error
			}

			event.Payload = newPayload
			return next(topic, event)
		}
	}
}
