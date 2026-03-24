package pipeline

import (
	"encoding/json"
	"testing"
	"time"

	"nephtys/internal/domain"
)

func TestBatchMiddleware_SizeFlush(t *testing.T) {
	cfg := &domain.BatchConfig{
		Enabled:       true,
		MaxBatchSize:  2,
		FlushInterval: "1h", // Never flush by time
	}
	batch := NewBatch(cfg)

	events := []domain.StreamEvent{
		{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":1}`)},
		{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":2}`)},
		{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":3}`)},
	}

	var batched []domain.StreamEvent
	sink := func(topic string, e domain.StreamEvent) error {
		batched = append(batched, e)
		return nil
	}

	handler := batch(sink)

	_ = handler("topic", events[0])
	if len(batched) != 0 {
		t.Error("expected no flush at 1 event")
	}

	_ = handler("topic", events[1])
	if len(batched) != 1 {
		t.Fatal("expected flush at 2 events")
	}

	// Unmarshal payload
	var arr []interface{}
	if err := json.Unmarshal(batched[0].Payload, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 2 {
		t.Errorf("expected 2 elements in batched payload, got %d", len(arr))
	}
	if batched[0].Type != "evt_batch" {
		t.Errorf("expected type evt_batch, got %v", batched[0].Type)
	}

	_ = handler("topic", events[2])
	if len(batched) != 1 {
		t.Error("expected no flush at 3rd event (1st in new batch)")
	}
}

func TestBatchMiddleware_TimeFlush(t *testing.T) {
	cfg := &domain.BatchConfig{
		Enabled:       true,
		MaxBatchSize:  10,
		FlushInterval: "50ms",
	}
	batch := NewBatch(cfg)

	var batched []domain.StreamEvent
	sink := func(topic string, e domain.StreamEvent) error {
		batched = append(batched, e)
		return nil
	}
	handler := batch(sink)

	_ = handler("topic", domain.StreamEvent{Source: "t", Type: "e", Payload: []byte(`{}`)})

	time.Sleep(100 * time.Millisecond)

	if len(batched) != 1 {
		t.Fatal("expected time-based flush to occur")
	}
}
