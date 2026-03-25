package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nephtys/internal/domain"
)

func TestWebhookSource_IDAndDefaults(t *testing.T) {
	src := NewWebhookSource("wh-id", "t", nil)
	if src.ID() != "wh-id" {
		t.Errorf("expected wh-id, got %s", src.ID())
	}
	// Nil config should default to port 8081 and path /webhook
	if src.config.Port != "8081" {
		t.Errorf("expected default port 8081, got %s", src.config.Port)
	}
	if src.config.Path != "/webhook" {
		t.Errorf("expected default path /webhook, got %s", src.config.Path)
	}

	// Empty config fields should also get defaults
	src2 := NewWebhookSource("wh-id2", "t", &domain.WebhookConfig{})
	if src2.config.Port != "8081" {
		t.Errorf("expected default port 8081, got %s", src2.config.Port)
	}

	// Path without leading slash should be prefixed
	src3 := NewWebhookSource("wh-id3", "t", &domain.WebhookConfig{Port: "9090", Path: "hook"})
	if src3.config.Path != "/hook" {
		t.Errorf("expected /hook, got %s", src3.config.Path)
	}
}

func TestWebhookSource(t *testing.T) {
	// Create common mock setup
	config := &domain.WebhookConfig{
		Port:      "8081",
		Path:      "/webhook",
		AuthToken: "secret123",
	}

	source := NewWebhookSource("test-webhook", "test.topic", config)

	// Capture published events
	var events []domain.StreamEvent
	publish := func(topic string, event domain.StreamEvent) error {
		events = append(events, event)
		return nil
	}

	handler := source.handleWebhook(publish)

	t.Run("Valid Authorized POST", func(t *testing.T) {
		events = nil // Reset
		payload := `{"foo": "bar"}`
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer secret123")

		w := httptest.NewRecorder()
		handler(w, req)

		res := w.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusAccepted {
			t.Errorf("Expected status 202, got %d", res.StatusCode)
		}

		if len(events) != 1 {
			t.Fatalf("Expected 1 event published, got %d", len(events))
		}

		if string(events[0].Payload) != payload {
			t.Errorf("Expected payload %s, got %s", payload, string(events[0].Payload))
		}
	})

	t.Run("Unauthorized - Missing Token", func(t *testing.T) {
		events = nil
		payload := `{"foo": "bar"}`
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
		// No auth header

		w := httptest.NewRecorder()
		handler(w, req)

		res := w.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", res.StatusCode)
		}
		if len(events) != 0 {
			t.Errorf("Expected 0 events, got %d", len(events))
		}
	})

	t.Run("Unauthorized - Wrong Token", func(t *testing.T) {
		events = nil
		payload := `{"foo": "bar"}`
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer wrong123")

		w := httptest.NewRecorder()
		handler(w, req)

		res := w.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", res.StatusCode)
		}
	})

	t.Run("Invalid HTTP Method", func(t *testing.T) {
		events = nil
		req := httptest.NewRequest(http.MethodGet, "/webhook", nil)

		w := httptest.NewRecorder()
		handler(w, req)

		res := w.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("Expected status 405, got %d", res.StatusCode)
		}
	})

	t.Run("Empty Body", func(t *testing.T) {
		events = nil
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer secret123")

		w := httptest.NewRecorder()
		handler(w, req)

		res := w.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("Expected status 400, got %d", res.StatusCode)
		}
	})

	t.Run("Non-JSON Payload", func(t *testing.T) {
		events = nil
		payload := `just some string`
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer secret123")

		w := httptest.NewRecorder()
		handler(w, req)

		res := w.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusAccepted {
			t.Errorf("Expected status 202, got %d", res.StatusCode)
		}

		if len(events) != 1 {
			t.Fatalf("Expected 1 event published, got %d", len(events))
		}

		// It should be json-escaped
		expected := `"` + payload + `"`
		if string(events[0].Payload) != expected {
			t.Errorf("Expected payload %s, got %s", expected, string(events[0].Payload))
		}
	})
}

func TestWebhookSource_Lifecycle(t *testing.T) {
	config := &domain.WebhookConfig{
		Port: "18081",
		Path: "/webhook",
	}

	source := NewWebhookSource("test-webhook-lifecycle", "test.topic", config)

	if source.Status() != domain.StatusIdle {
		t.Errorf("Expected status idle, got %s", source.Status())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run start in a goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- source.Start(ctx, func(topic string, event domain.StreamEvent) error { return nil })
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	if source.Status() != domain.StatusRunning {
		t.Errorf("Expected status running, got %s", source.Status())
	}

	// Trigger shutdown
	source.Stop()

	err := <-errCh
	if err != nil && err != context.Canceled {
		t.Errorf("Expected canceled error, got %v", err)
	}

	if source.Status() != domain.StatusStopped {
		t.Errorf("Expected status stopped, got %s", source.Status())
	}
}
