package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// A payload that is not valid JSON makes marshalling the batch array fail.
// json.RawMessage passes its bytes through verbatim, so the failure surfaces
// only at the array level — the batch must be reported rather than published
// as a malformed event.
func TestBatchMiddleware_MarshalFailureIsReported(t *testing.T) {
	cfg := &domain.BatchConfig{Enabled: true, MaxBatchSize: 1, FlushInterval: "1h"}
	batch := NewBatch(context.Background(), cfg)

	published := make(chan domain.StreamEvent, 1)
	handler := batch(func(topic string, e domain.StreamEvent) error {
		published <- e
		return nil
	})

	err := handler("topic", domain.StreamEvent{
		Source:  "test",
		Type:    "evt",
		Payload: json.RawMessage(`not json at all`),
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	select {
	case e := <-published:
		t.Fatalf("unmarshalable batch was published anyway: %s", e.Payload)
	case <-time.After(200 * time.Millisecond):
		// Correct: the batch was dropped and logged, not published.
	}

	// The worker must survive the failure and keep batching valid events.
	if err := handler("topic", domain.StreamEvent{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":1}`)}); err != nil {
		t.Fatalf("handler returned error after a failed batch: %v", err)
	}
	select {
	case e := <-published:
		if string(e.Payload) != `[{"id":1}]` {
			t.Errorf("payload = %s, want [{\"id\":1}]", e.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("worker stopped batching after a marshal failure")
	}
}

// max_batch_size is a contract, and the final drain must honour it rather than
// emitting one oversized batch on shutdown.
func TestBatchMiddleware_DrainRespectsMaxBatchSize(t *testing.T) {
	const maxSize = 2
	cfg := &domain.BatchConfig{Enabled: true, MaxBatchSize: maxSize, FlushInterval: "1h"}
	ctx, cancel := context.WithCancel(context.Background())
	batch := NewBatch(ctx, cfg)

	release := make(chan struct{})
	flushed := make(chan domain.StreamEvent, 4)
	var once sync.Once
	handler := batch(func(topic string, e domain.StreamEvent) error {
		// Block the very first flush so the worker is busy while we fill the
		// channel buffer behind it, then let everything proceed.
		once.Do(func() { <-release })
		flushed <- e
		return nil
	})

	send := func(id int) {
		payload := json.RawMessage(`{"id":` + strconv.Itoa(id) + `}`)
		if err := handler("topic", domain.StreamEvent{Source: "test", Type: "evt", Payload: payload}); err != nil {
			t.Errorf("event %d: handler returned error: %v", id, err)
		}
	}

	// Fill one batch so the worker enters the blocked flush.
	send(1)
	send(2)
	// These land in the channel buffer while the worker is stuck.
	send(3)
	send(4)

	cancel()
	close(release)

	var total int
	for total < 4 {
		select {
		case e := <-flushed:
			var payloads []json.RawMessage
			if err := json.Unmarshal(e.Payload, &payloads); err != nil {
				t.Fatalf("unmarshal batched payload: %v", err)
			}
			if len(payloads) > maxSize {
				t.Errorf("flushed a batch of %d, exceeding max_batch_size %d", len(payloads), maxSize)
			}
			total += len(payloads)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 4 events were flushed", total)
		}
	}
}

// An unset or non-positive max_batch_size falls back to the documented default
// rather than producing a zero-capacity channel.
func TestBatchMiddleware_DefaultMaxBatchSize(t *testing.T) {
	cfg := &domain.BatchConfig{Enabled: true, MaxBatchSize: 0, FlushInterval: "50ms"}
	batch := NewBatch(context.Background(), cfg)

	flushed := make(chan domain.StreamEvent, 1)
	handler := batch(func(topic string, e domain.StreamEvent) error {
		flushed <- e
		return nil
	})

	// With a zero-capacity channel this send would block forever.
	if err := handler("topic", domain.StreamEvent{Source: "test", Type: "evt", Payload: json.RawMessage(`{"id":1}`)}); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("event was never flushed by the interval timer")
	}
}

// A downstream publish failure must not wedge the worker: the batch is logged
// and cleared, and subsequent batches still flow.
func TestBatchMiddleware_FlushErrorDoesNotStallWorker(t *testing.T) {
	cfg := &domain.BatchConfig{Enabled: true, MaxBatchSize: 1, FlushInterval: "1h"}
	batch := NewBatch(context.Background(), cfg)

	seen := make(chan domain.StreamEvent, 2)
	var calls atomic.Int32
	handler := batch(func(topic string, e domain.StreamEvent) error {
		seen <- e
		if calls.Add(1) == 1 {
			return errors.New("broker unavailable")
		}
		return nil
	})

	for i := 1; i <= 2; i++ {
		payload := json.RawMessage(`{"id":` + strconv.Itoa(i) + `}`)
		if err := handler("topic", domain.StreamEvent{Source: "test", Type: "evt", Payload: payload}); err != nil {
			t.Fatalf("event %d: handler returned error: %v", i, err)
		}
	}

	for i := 1; i <= 2; i++ {
		select {
		case <-seen:
		case <-time.After(2 * time.Second):
			t.Fatalf("batch %d never reached the sink; worker stalled after the failure", i)
		}
	}
}
