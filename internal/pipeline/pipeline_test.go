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

	var received domain.StreamEvent
	publisher := func(topic string, e domain.StreamEvent) error {
		received = e
		return nil
	}

	handler := pipe.Execute(publisher)
	_ = handler("topic", event)

	if received.Source != "test" {
		t.Errorf("expected source 'test', got %q", received.Source)
	}
}

func TestPipeline_SingleMiddleware(t *testing.T) {
	upper := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			e.Source = e.Source + "_processed"
			return next(topic, e)
		}
	}

	pipe := New(upper)
	var received domain.StreamEvent
	publisher := func(topic string, e domain.StreamEvent) error {
		received = e
		return nil
	}

	handler := pipe.Execute(publisher)
	_ = handler("topic", makeEvent("raw", "ws"))

	if received.Source != "raw_processed" {
		t.Errorf("expected 'raw_processed', got %q", received.Source)
	}
}

func TestPipeline_ChainOrder(t *testing.T) {
	a := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			e.Source = e.Source + "_a"
			return next(topic, e)
		}
	}
	b := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			e.Source = e.Source + "_b"
			return next(topic, e)
		}
	}

	pipe := New(a, b)
	var received domain.StreamEvent
	publisher := func(topic string, e domain.StreamEvent) error {
		received = e
		return nil
	}

	handler := pipe.Execute(publisher)
	_ = handler("topic", makeEvent("x", "ws"))

	if received.Source != "x_a_b" {
		t.Errorf("expected 'x_a_b', got %q — middleware order wrong", received.Source)
	}
}

func TestPipeline_DropEvent(t *testing.T) {
	dropper := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			return nil // drop everything
		}
	}
	neverReached := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			t.Fatal("this middleware should never execute after a drop")
			return next(topic, e)
		}
	}

	pipe := New(dropper, neverReached)
	published := false
	publisher := func(topic string, e domain.StreamEvent) error {
		published = true
		return nil
	}

	handler := pipe.Execute(publisher)
	_ = handler("topic", makeEvent("x", "ws"))

	if published {
		t.Fatal("event should have been dropped")
	}
}

func TestPipeline_FilterByType(t *testing.T) {
	filter := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			if e.Type == "websocket_message" {
				return next(topic, e)
			}
			return nil
		}
	}

	pipe := New(filter)
	published := false
	publisher := func(topic string, e domain.StreamEvent) error {
		published = true
		return nil
	}

	handler := pipe.Execute(publisher)

	_ = handler("topic", makeEvent("x", "websocket_message"))
	if !published {
		t.Error("websocket_message should pass the filter")
	}

	published = false
	_ = handler("topic", makeEvent("x", "heartbeat"))
	if published {
		t.Error("heartbeat should be dropped by the filter")
	}
}
