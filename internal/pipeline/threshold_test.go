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

func TestThresholdMiddleware_GroupBy(t *testing.T) {
	cfg := &domain.ThresholdConfig{
		Enabled: true,
		Path:    "pm25",
		Delta:   1.0,
		GroupBy: "station",
	}
	threshold := NewThreshold("test", cfg)

	events := []domain.StreamEvent{
		{Payload: json.RawMessage(`{"station":"AQ-001","pm25":10.0}`)},
		{Payload: json.RawMessage(`{"station":"AQ-002","pm25":20.0}`)},
		{Payload: json.RawMessage(`{"station":"AQ-001","pm25":10.4}`)},
		{Payload: json.RawMessage(`{"station":"AQ-002","pm25":21.2}`)},
	}
	expectedPasses := []bool{true, true, false, true}

	passedCount := 0
	handler := threshold(func(topic string, e domain.StreamEvent) error {
		passedCount++
		return nil
	})

	for i, e := range events {
		before := passedCount
		_ = handler("topic", e)
		passed := passedCount > before
		if passed != expectedPasses[i] {
			t.Errorf("event %d: passed=%v, want=%v", i, passed, expectedPasses[i])
		}
	}
}
