package pipeline

import (
	"encoding/json"
	"testing"

	"nephtys/internal/domain"
)

func makeEvent(source, typ string) domain.StreamEvent {
	return domain.StreamEvent{
		Source:    source,
		Type:      typ,
		Timestamp: 1700000000000,
		Payload:   json.RawMessage(`{"price":"42000"}`),
	}
}

func TestPipeline_Passthrough(t *testing.T) {
	pipe := New()
	event := makeEvent("test", "ws")

	result, ok := pipe.Process(event)
	if !ok {
		t.Fatal("passthrough pipeline should not drop events")
	}
	if result.Source != "test" {
		t.Errorf("expected source 'test', got %q", result.Source)
	}
}

func TestPipeline_SingleMiddleware(t *testing.T) {
	// Middleware that uppercases the source field
	upper := func(e domain.StreamEvent) (domain.StreamEvent, bool) {
		e.Source = e.Source + "_processed"
		return e, true
	}

	pipe := New(upper)
	result, ok := pipe.Process(makeEvent("raw", "ws"))

	if !ok {
		t.Fatal("event should not be dropped")
	}
	if result.Source != "raw_processed" {
		t.Errorf("expected 'raw_processed', got %q", result.Source)
	}
}

func TestPipeline_ChainOrder(t *testing.T) {
	// Two middlewares: first appends "_a", second appends "_b"
	a := func(e domain.StreamEvent) (domain.StreamEvent, bool) {
		e.Source = e.Source + "_a"
		return e, true
	}
	b := func(e domain.StreamEvent) (domain.StreamEvent, bool) {
		e.Source = e.Source + "_b"
		return e, true
	}

	pipe := New(a, b)
	result, ok := pipe.Process(makeEvent("x", "ws"))

	if !ok {
		t.Fatal("event should not be dropped")
	}
	if result.Source != "x_a_b" {
		t.Errorf("expected 'x_a_b', got %q — middleware order wrong", result.Source)
	}
}

func TestPipeline_DropEvent(t *testing.T) {
	dropper := func(e domain.StreamEvent) (domain.StreamEvent, bool) {
		return e, false // drop everything
	}
	neverReached := func(e domain.StreamEvent) (domain.StreamEvent, bool) {
		t.Fatal("this middleware should never execute after a drop")
		return e, true
	}

	pipe := New(dropper, neverReached)
	_, ok := pipe.Process(makeEvent("x", "ws"))

	if ok {
		t.Fatal("event should have been dropped")
	}
}

func TestPipeline_FilterByType(t *testing.T) {
	// Only allow "websocket_message" events through
	filter := func(e domain.StreamEvent) (domain.StreamEvent, bool) {
		return e, e.Type == "websocket_message"
	}

	pipe := New(filter)

	_, ok := pipe.Process(makeEvent("x", "websocket_message"))
	if !ok {
		t.Error("websocket_message should pass the filter")
	}

	_, ok = pipe.Process(makeEvent("x", "heartbeat"))
	if ok {
		t.Error("heartbeat should be dropped by the filter")
	}
}
