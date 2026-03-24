package pipeline

import (
	"encoding/json"
	"testing"

	"nephtys/internal/domain"
)

func TestThresholdMiddleware(t *testing.T) {
	cfg := &domain.ThresholdConfig{
		Enabled: true,
		Path:    "data.val",
		Delta:   0.5,
	}
	threshold := NewThreshold("test", cfg)

	events := []domain.StreamEvent{
		{Source: "s1", Payload: json.RawMessage(`{"data":{"val":10.0}}`)},
		{Source: "s1", Payload: json.RawMessage(`{"data":{"val":10.2}}`)}, // Drop (< 0.5)
		{Source: "s1", Payload: json.RawMessage(`{"data":{"val":10.6}}`)}, // Pass (diff 0.6)
		{Source: "s1", Payload: json.RawMessage(`{"data":{"val":10.6}}`)}, // Drop (no change)
		{Source: "s1", Payload: json.RawMessage(`{"data":{"val":11.2}}`)}, // Pass (diff 0.6)
	}

	passedCount := 0
	sink := func(topic string, e domain.StreamEvent) error {
		passedCount++
		return nil
	}
	handler := threshold(sink)

	expectedPasses := []bool{true, false, true, false, true}

	for i, e := range events {
		before := passedCount
		_ = handler("topic", e)
		passed := passedCount > before
		if passed != expectedPasses[i] {
			t.Errorf("event %d: passed=%v, want=%v", i, passed, expectedPasses[i])
		}
	}
}
