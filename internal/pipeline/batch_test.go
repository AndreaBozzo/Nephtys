package pipeline

import (
	"encoding/json"
	"sync"
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

	var mu sync.Mutex
	var batched []domain.StreamEvent
	sink := func(topic string, e domain.StreamEvent) error {
		mu.Lock()
		defer mu.Unlock()
		batched = append(batched, e)
		return nil
	}

	handler := batch(sink)

	_ = handler("topic", events[0])
	mu.Lock()
	if len(batched) != 0 {
		mu.Unlock()
		t.Error("expected no flush at 1 event")
	} else {
		mu.Unlock()
	}

	_ = handler("topic", events[1])
	mu.Lock()
	if len(batched) != 1 {
		mu.Unlock()
		t.Fatal("expected flush at 2 events")
	}

	// Unmarshal payload
	var arr []interface{}
	if err := json.Unmarshal(batched[0].Payload, &arr); err != nil {
		mu.Unlock()
		t.Fatal(err)
	}
	if len(arr) != 2 {
		t.Errorf("expected 2 elements in batched payload, got %d", len(arr))
	}
	if batched[0].Type != "evt_batch" {
		t.Errorf("expected type evt_batch, got %v", batched[0].Type)
	}
	mu.Unlock()

	_ = handler("topic", events[2])
	mu.Lock()
	if len(batched) != 1 {
		mu.Unlock()
		t.Error("expected no flush at 3rd event (1st in new batch)")
	} else {
		mu.Unlock()
	}
}

func TestBatchMiddleware_TimeFlush(t *testing.T) {
	cfg := &domain.BatchConfig{
		Enabled:       true,
		MaxBatchSize:  10,
		FlushInterval: "50ms",
	}
	batch := NewBatch(cfg)

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
