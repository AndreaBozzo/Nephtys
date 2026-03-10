package connector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
)

func TestSSESource_Success(t *testing.T) {
	// Mock SSE server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("Expected http.ResponseWriter to be an http.Flusher")
		}

		// Send an event with type
		fmt.Fprintf(w, "event: custom_event\n")
		fmt.Fprintf(w, "data: {\"foo\":\"bar\"}\n\n")
		flusher.Flush()

		// Send a simple data event (no type)
		fmt.Fprintf(w, "data: {\"msg\":\"hello\"}\n\n")
		flusher.Flush()

		// Send multi-line data (must evaluate to valid JSON)
		fmt.Fprintf(w, "data: {\n")
		fmt.Fprintf(w, "data: \"multi\":\"\\nline\"}\n\n")
		flusher.Flush()

		// Keep connection open
		<-r.Context().Done()
	}))
	defer ts.Close()

	config := &domain.SseConfig{
		Headers: map[string]string{
			"Authorization": "Bearer token",
		},
	}
	source := connector.NewSSESource("test-sse", ts.URL, "test-topic", config)

	if source.ID() != "test-sse" {
		t.Errorf("Expected ID 'test-sse', got %q", source.ID())
	}
	if source.Status() != domain.StatusIdle {
		t.Errorf("Expected StatusIdle, got %v", source.Status())
	}

	events := make(chan domain.StreamEvent, 5)
	publish := func(topic string, event domain.StreamEvent) error {
		if topic != "test-topic" {
			t.Errorf("Expected topic 'test-topic', got %q", topic)
		}
		events <- event
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run source in a goroutine
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	// Wait for connection
	time.Sleep(100 * time.Millisecond)
	if source.Status() != domain.StatusRunning {
		t.Errorf("Expected StatusRunning, got %v", source.Status())
	}

	verifyEvent := func(expectedType string, expectedPayload func(map[string]any) bool) {
		select {
		case event := <-events:
			t.Logf("Received event: Type=%s, Payload(len=%d)=%s", event.Type, len(event.Payload), string(event.Payload))
			if event.Source != "test-sse" {
				t.Errorf("Expected source 'test-sse', got %q", event.Source)
			}
			if event.Type != expectedType {
				t.Errorf("Expected type %q, got %q", expectedType, event.Type)
			}
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("Failed to unmarshal payload: %v | raw: %s", err, string(event.Payload))
			}
			if !expectedPayload(payload) {
				t.Errorf("Payload assertion failed for: %s", string(event.Payload))
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("Timeout waiting for event %q", expectedType)
		}
	}

	// 1. First event
	verifyEvent("custom_event", func(p map[string]any) bool {
		return p["foo"] == "bar"
	})

	// 2. Second event
	verifyEvent("sse_message", func(p map[string]any) bool {
		return p["msg"] == "hello"
	})

	// 3. Third event (multi-line)
	verifyEvent("sse_message", func(p map[string]any) bool {
		return p["multi"] == "\nline"
	})

	// Stop source
	source.Stop()

	// Wait for goroutine to exit
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for Start to return")
	}

	if source.Status() != domain.StatusStopped {
		t.Errorf("Expected StatusStopped, got %v", source.Status())
	}
}

func TestSSESource_Reconnect(t *testing.T) {
	connectCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectCount++
		if connectCount == 1 {
			// Fail the first connection
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"status\":\"ok\"}\n\n")
		flusher.Flush()

		// Keep alive until test interrupts
		<-r.Context().Done()
	}))
	defer ts.Close()

	source := connector.NewSSESource("test-reconnect", ts.URL, "test-topic", nil)

	events := make(chan domain.StreamEvent, 1)
	publish := func(_ string, evt domain.StreamEvent) error {
		events <- evt
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run source
	go func() {
		_ = source.Start(ctx, publish)
	}()

	// Wait for successful event from second connection
	select {
	case evt := <-events:
		var p map[string]any
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			t.Errorf("Failed to unmarshal reconnect payload: %v", err)
		}
		if p["status"] != "ok" {
			t.Errorf("Unexpected payload: %s", string(evt.Payload))
		}
	case <-time.After(3 * time.Second): // Account for backoff
		t.Fatal("Timeout waiting for event after reconnect")
	}

	if connectCount < 2 {
		t.Errorf("Expected at least 2 connections (1 failed + 1 successful), got %d", connectCount)
	}

	source.Stop()
}
