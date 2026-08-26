package connector_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
)

func TestRESTPollerSource(t *testing.T) {
	// Create a test server that returns JSON
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Header") != "test-value" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		response := map[string]string{"message": "hello"}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	config := &domain.RestPollerConfig{
		Interval: "100ms", // Short interval for testing
		Method:   "GET",
		Headers: map[string]string{
			"X-Test-Header": "test-value",
		},
	}

	source := connector.NewRESTPollerSource("test-poller", ts.URL, "test-topic", config)

	if source.ID() != "test-poller" {
		t.Errorf("Expected ID 'test-poller', got %q", source.ID())
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
	done := make(chan error, 1)
	go func() {
		done <- source.Run(ctx, publish, func() {})
	}()

	// Wait for an event to be published
	select {
	case event := <-events:
		if event.Source != "test-poller" {
			t.Errorf("Expected event source 'test-poller', got %q", event.Source)
		}
		if event.Type != "rest_poller_response" {
			t.Errorf("Expected event type 'rest_poller_response', got %q", event.Type)
		}

		var payload map[string]string
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("Failed to unmarshal payload: %v", err)
		}
		if payload["message"] != "hello" {
			t.Errorf("Expected message 'hello', got %q", payload["message"])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for event")
	}

	// Wait for a second event to prove we are polling
	select {
	case <-events:
		// Success
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for second event")
	}

	// Stop the source
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

func TestRESTPollerSource_NonJSONResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("plain text response"))
	}))
	defer ts.Close()

	config := &domain.RestPollerConfig{Interval: "100ms", Method: "GET"}
	source := connector.NewRESTPollerSource("poller-text", ts.URL, "test.topic", config)

	events := make(chan domain.StreamEvent, 5)
	publish := func(topic string, event domain.StreamEvent) error {
		events <- event
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := source.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()
	go func() { _ = source.Run(ctx, publish, func() {}) }()

	select {
	case evt := <-events:
		// Non-JSON body should be wrapped as a JSON string
		if len(evt.Payload) == 0 {
			t.Error("expected non-empty payload for non-JSON response")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

func TestRESTPollerSource_ErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	config := &domain.RestPollerConfig{Interval: "100ms", Method: "GET"}
	source := connector.NewRESTPollerSource("poller-err", ts.URL, "test.topic", config)

	events := make(chan domain.StreamEvent, 5)
	publish := func(topic string, event domain.StreamEvent) error {
		events <- event
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := source.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()
	go func() { _ = source.Run(ctx, publish, func() {}) }()

	// Should not receive events on 500 status
	select {
	case <-events:
		t.Fatal("should not have received event on 500 status")
	case <-time.After(300 * time.Millisecond):
		// OK - no events expected
	}
	cancel()
}

func TestRESTPollerSource_EmptyResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// empty body
	}))
	defer ts.Close()

	config := &domain.RestPollerConfig{Interval: "100ms", Method: "GET"}
	source := connector.NewRESTPollerSource("poller-empty", ts.URL, "test.topic", config)

	events := make(chan domain.StreamEvent, 5)
	publish := func(topic string, event domain.StreamEvent) error {
		events <- event
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := source.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()
	go func() { _ = source.Run(ctx, publish, func() {}) }()

	// Should not receive events for empty body
	select {
	case <-events:
		t.Fatal("should not have received event for empty body")
	case <-time.After(300 * time.Millisecond):
		// OK
	}
	cancel()
}

// TestRESTPollerSource_InvalidInterval checks the one local resource this
// source acquires. An unparseable interval is a property of the config, so it
// belongs to Open, where whoever registered the stream is still waiting for an
// answer.
func TestRESTPollerSource_InvalidInterval(t *testing.T) {
	source := connector.NewRESTPollerSource("test", "http://example.com", "topic",
		&domain.RestPollerConfig{Interval: "invalid"})

	err := source.Open(context.Background())
	if err == nil {
		t.Fatal("Expected error for invalid interval, got nil")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error %q does not name the offending value", err)
	}
}

// TestRESTPollerSource_ReadyAfterFirstSuccessfulPoll pins where readiness
// comes from. Reporting it on entry would mean a poller claims to be running
// before it has ever reached its endpoint, which is the one connector that
// used to do that.
func TestRESTPollerSource_ReadyAfterFirstSuccessfulPoll(t *testing.T) {
	polled := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case polled <- struct{}{}:
		default:
		}
		_, _ = w.Write([]byte(`{"message":"hello"}`))
	}))
	defer ts.Close()

	source := connector.NewRESTPollerSource("poller-ready", ts.URL, "test.topic",
		&domain.RestPollerConfig{Interval: "1h", Method: "GET"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := source.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()

	ready := make(chan struct{})
	go func() {
		_ = source.Run(ctx, func(string, domain.StreamEvent) error { return nil }, func() { close(ready) })
	}()

	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("source never polled")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("source polled successfully but never reported ready")
	}
}

// TestRESTPollerSource_NotReadyWhileEndpointFails is the other half: a poller
// whose URL never answers must not report itself running. Its stream stays
// connecting, so nothing reports it healthy while it has ingested nothing.
func TestRESTPollerSource_NotReadyWhileEndpointFails(t *testing.T) {
	attempts := make(chan struct{}, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer ts.Close()

	source := connector.NewRESTPollerSource("poller-404", ts.URL, "test.topic",
		&domain.RestPollerConfig{Interval: "20ms", Method: "GET"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := source.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()

	var reported atomic.Bool
	go func() {
		_ = source.Run(ctx, func(string, domain.StreamEvent) error { return nil }, func() { reported.Store(true) })
	}()

	// Several polls have to have been attempted and refused.
	for i := 0; i < 3; i++ {
		select {
		case <-attempts:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d poll attempts reached the endpoint", i)
		}
	}
	if reported.Load() {
		t.Error("source reported ready while every poll was failing")
	}
}
