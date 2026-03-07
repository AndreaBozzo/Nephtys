package connector_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	source.Stop()

	// Wait for goroutine to exit
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Expected context.Canceled error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for Start to return")
	}

	if source.Status() != domain.StatusStopped {
		t.Errorf("Expected StatusStopped, got %v", source.Status())
	}
}

func TestRESTPollerSource_InvalidInterval(t *testing.T) {
	config := &domain.RestPollerConfig{
		Interval: "invalid",
	}

	source := connector.NewRESTPollerSource("test", "http://example.com", "topic", config)
	err := source.Start(context.Background(), func(t string, e domain.StreamEvent) error { return nil })

	if err == nil {
		t.Fatal("Expected error for invalid interval, got nil")
	}
	if source.Status() != domain.StatusError {
		t.Errorf("Expected StatusError, got %v", source.Status())
	}
}
