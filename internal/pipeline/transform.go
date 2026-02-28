package pipeline

import (
	"encoding/json"
	"log/slog"
	"strings"

	"nephtys/internal/domain"
)

// NewTransform creates a middleware that restructures a JSON payload.
// It uses a map of "new_key" -> "dot.notation.path" to extract values
// from the original payload and construct a new, flat JSON object.
func NewTransform(cfg *domain.TransformConfig) Middleware {
	if cfg == nil || len(cfg.Mapping) == 0 {
		return nil
	}

	return func(event domain.StreamEvent) (domain.StreamEvent, bool) {
		if len(event.Payload) == 0 {
			return event, true
		}

		var original map[string]interface{}
		if err := json.Unmarshal(event.Payload, &original); err != nil {
			slog.Debug("Transform: payload is not a JSON object, skipping", "source", event.Source)
			return event, true
		}

		transformed := make(map[string]interface{}, len(cfg.Mapping))
		for newKey, path := range cfg.Mapping {
			if val, ok := extractValue(original, path); ok {
				transformed[newKey] = val
			}
		}

		// Repack
		newPayload, err := json.Marshal(transformed)
		if err != nil {
			slog.Error("Transform: failed to marshal transformed payload", "error", err)
			return event, true
		}

		event.Payload = newPayload
		return event, true
	}
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
