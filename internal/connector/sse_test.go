package connector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		if _, err := fmt.Fprintf(w, "event: custom_event\n"); err != nil {
			t.Errorf("failed to write SSE event type: %v", err)
			return
		}
		if _, err := fmt.Fprintf(w, "data: {\"foo\":\"bar\"}\n\n"); err != nil {
			t.Errorf("failed to write SSE event payload: %v", err)
			return
		}
		flusher.Flush()

		// Send a simple data event (no type)
		if _, err := fmt.Fprintf(w, "data: {\"msg\":\"hello\"}\n\n"); err != nil {
			t.Errorf("failed to write SSE message: %v", err)
			return
		}
		flusher.Flush()

		// Send multi-line data (must evaluate to valid JSON)
		if _, err := fmt.Fprintf(w, "data: {\n"); err != nil {
			t.Errorf("failed to write SSE multiline prefix: %v", err)
			return
		}
		if _, err := fmt.Fprintf(w, "data: \"multi\":\"\\nline\"}\n\n"); err != nil {
			t.Errorf("failed to write SSE multiline payload: %v", err)
			return
		}
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

	if err := source.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()

	// Run source in a goroutine
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- source.Run(ctx, publish, func() { close(ready) })
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("source never reported ready")
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
	cancel()

	// Wait for goroutine to exit
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancellation returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for Run to return")
	}
}

// TestSSESource_CleanEOFEndsSessionWithAReason covers an event stream that
// simply ends. The scanner reports no error, but the session is over and the
// supervisor is about to spend an attempt on it — returning nil would spend
// that attempt with nothing recorded to say why, leaving a stream that can
// reach a terminal state with an empty last_error.
func TestSSESource_CleanEOFEndsSessionWithAReason(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprint(w, "data: {\"n\":1}\n\n"); err != nil {
			t.Errorf("write event: %v", err)
		}
		// Returning closes the body: a clean end of stream.
	}))
	defer ts.Close()

	// A token in the query string is the thing that must not come back out in
	// the error, since last_error is served over the API.
	source := connector.NewSSESource("sse-eof", ts.URL+"?apikey=TOPSECRET", "test.topic", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := source.Run(ctx, func(string, domain.StreamEvent) error { return nil }, func() {})
	if err == nil {
		t.Fatal("a stream that ended on its own reported no error")
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("error %q leaks the endpoint credential", err)
	}
}

// TestSSESource_SessionEndsOnBadStatus covers the failure the supervisor turns
// into a reconnect: a non-2xx response ends the session with an error instead
// of being retried inside the connector, and the next session succeeds.
func TestSSESource_SessionEndsOnBadStatus(t *testing.T) {
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
		if _, err := fmt.Fprintf(w, "data: {\"status\":\"ok\"}\n\n"); err != nil {
			t.Errorf("failed to write reconnect SSE message: %v", err)
			return
		}
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

	// First session: the endpoint answers 503, so Run reports the failure
	// rather than swallowing it in a retry loop of its own.
	err := source.Run(ctx, publish, func() { t.Error("ready fired on a 503 response") })
	if err == nil {
		t.Fatal("Run against a 503 endpoint returned nil")
	}

	// Second session: the same source connects and delivers.
	go func() {
		_ = source.Run(ctx, publish, func() {})
	}()

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
}
