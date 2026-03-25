package pipeline

import (
	"context"
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
	batch := NewBatch(context.Background(), cfg)

	events := []domain.StreamEvent{
		{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":1}`)},
		{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":2}`)},
		{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":3}`)},
	}

	batched := make(chan domain.StreamEvent, 1)
	sink := func(topic string, e domain.StreamEvent) error {
		batched <- e
		return nil
	}

	handler := batch(sink)

	_ = handler("topic", events[0])
	select {
	case <-batched:
		t.Fatal("expected no flush at 1 event")
	case <-time.After(50 * time.Millisecond):
		// OK
	}

	_ = handler("topic", events[1])
	select {
	case e := <-batched:
		// Unmarshal payload
		var arr []interface{}
		if err := json.Unmarshal(e.Payload, &arr); err != nil {
			t.Fatal(err)
		}
		if len(arr) != 2 {
			t.Errorf("expected 2 elements in batched payload, got %d", len(arr))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected flush at 2 events")
	}

	_ = handler("topic", events[2])
	select {
	case <-batched:
		t.Fatal("expected no flush at 3rd event")
	case <-time.After(50 * time.Millisecond):
		// OK
	}
}

func TestBatchMiddleware_TimeFlush(t *testing.T) {
	cfg := &domain.BatchConfig{
		Enabled:       true,
		MaxBatchSize:  10,
		FlushInterval: "50ms",
	}
	batch := NewBatch(context.Background(), cfg)

	batched := make(chan domain.StreamEvent, 1)
	sink := func(topic string, e domain.StreamEvent) error {
		batched <- e
		return nil
	}
	handler := batch(sink)

	_ = handler("topic", domain.StreamEvent{Source: "t", Type: "e", Payload: []byte(`{}`)})

	select {
	case e := <-batched:
		if e.Type != "e_batch" {
			t.Errorf("expected type e_batch, got %v", e.Type)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for time-based flush")
	}
}
