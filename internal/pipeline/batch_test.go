package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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

// Binary events carry their bytes on Data with Payload nil. Aggregating one
// into the JSON array envelope would publish `null` and discard the data
// entirely, so the middleware must pass them through untouched.
func TestBatchMiddleware_BinaryPassesThroughIntact(t *testing.T) {
	cfg := &domain.BatchConfig{
		Enabled:       true,
		MaxBatchSize:  100,
		FlushInterval: "1h", // Never flush by time.
	}
	batch := NewBatch(context.Background(), cfg)

	got := make(chan domain.StreamEvent, 4)
	handler := batch(func(topic string, e domain.StreamEvent) error {
		got <- e
		return nil
	})

	frames := [][]byte{{0x01, 0x02, 0x03}, {0xff, 0x00, 0xfe}}
	for _, frame := range frames {
		err := handler("topic", domain.StreamEvent{
			Source:      "test",
			Type:        "websocket_binary",
			ContentType: domain.ContentTypeBinary,
			Data:        frame,
		})
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
	}

	for i, want := range frames {
		select {
		case e := <-got:
			if e.ContentType != domain.ContentTypeBinary {
				t.Errorf("frame %d: content type = %q, want %q", i, e.ContentType, domain.ContentTypeBinary)
			}
			if !bytes.Equal(e.Data, want) {
				t.Errorf("frame %d: Data = %v, want %v", i, e.Data, want)
			}
			if strings.HasSuffix(e.Type, "_batch") {
				t.Errorf("frame %d: type %q was batched, want passthrough", i, e.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("frame %d never reached the sink", i)
		}
	}
}

// Cancelling the pipeline context (shutdown, or a pipeline hot-swap) must not
// discard events already accepted into the channel buffer — the source was
// told they were published.
func TestBatchMiddleware_CancelDrainsBufferedEvents(t *testing.T) {
	cfg := &domain.BatchConfig{
		Enabled:       true,
		MaxBatchSize:  100, // Large enough that nothing flushes by size.
		FlushInterval: "1h",
	}
	ctx, cancel := context.WithCancel(context.Background())
	batch := NewBatch(ctx, cfg)

	got := make(chan domain.StreamEvent, 1)
	handler := batch(func(topic string, e domain.StreamEvent) error {
		got <- e
		return nil
	})

	const sent = 5
	for i := range sent {
		payload := json.RawMessage(`{"id":` + strconv.Itoa(i) + `}`)
		if err := handler("topic", domain.StreamEvent{Source: "test", Type: "evt", Payload: payload}); err != nil {
			t.Fatalf("event %d: handler returned error: %v", i, err)
		}
	}

	cancel()

	select {
	case e := <-got:
		var payloads []json.RawMessage
		if err := json.Unmarshal(e.Payload, &payloads); err != nil {
			t.Fatalf("unmarshal batched payload: %v", err)
		}
		if len(payloads) != sent {
			t.Errorf("flushed %d events on cancel, want %d", len(payloads), sent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not flush the buffered events")
	}
}

// Once the context is cancelled the handler must reject new events rather than
// park them in a buffer whose worker has exited.
func TestBatchMiddleware_RejectsAfterCancel(t *testing.T) {
	cfg := &domain.BatchConfig{Enabled: true, MaxBatchSize: 10, FlushInterval: "1h"}
	ctx, cancel := context.WithCancel(context.Background())
	batch := NewBatch(ctx, cfg)
	handler := batch(func(topic string, e domain.StreamEvent) error { return nil })

	cancel()
	// Give the worker a moment to observe cancellation and exit.
	time.Sleep(50 * time.Millisecond)

	err := handler("topic", domain.StreamEvent{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":1}`)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("handler after cancel returned %v, want context.Canceled", err)
	}
}
