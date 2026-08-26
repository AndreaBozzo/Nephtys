package connector

import (
	"context"
	"net"
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
		defer func() { _ = res.Body.Close() }()

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
		defer func() { _ = res.Body.Close() }()

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
		defer func() { _ = res.Body.Close() }()

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
		defer func() { _ = res.Body.Close() }()

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
		defer func() { _ = res.Body.Close() }()

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
		defer func() { _ = res.Body.Close() }()

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
	source := NewWebhookSource("test-webhook-lifecycle", "test.topic", &domain.WebhookConfig{
		Port: "0", // let the OS pick, so a busy port cannot flake the test
		Path: "/webhook",
	})

	if err := source.Open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()

	addr := source.Addr()
	if addr == nil {
		t.Fatal("Open returned nil error but bound no address")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- source.Run(ctx, func(string, domain.StreamEvent) error { return nil }, func() { close(ready) })
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("source never reported ready")
	}

	// ready means bound and serving, not merely "about to be": the endpoint
	// has to answer immediately.
	res, err := http.Post("http://"+addr.String()+"/webhook", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("post to a source that reported ready: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusAccepted)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run after cancellation returned %v, want nil", err)
	}
}

// TestWebhookSource_OpenReportsBindFailure is the connector half of the
// registration contract: a port that is already taken has to fail in Open,
// synchronously, so the caller that registered the stream is the one told.
func TestWebhookSource_OpenReportsBindFailure(t *testing.T) {
	// Bind the wildcard address, the same one the source binds. Holding only
	// 127.0.0.1:P does not conflict with 0.0.0.0:P on every platform, so a
	// loopback holder would make this test pass for the wrong reason.
	holder, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("bind holder: %v", err)
	}
	defer func() { _ = holder.Close() }()

	_, port, err := net.SplitHostPort(holder.Addr().String())
	if err != nil {
		t.Fatalf("split holder addr: %v", err)
	}

	source := NewWebhookSource("taken", "test.topic", &domain.WebhookConfig{Port: port, Path: "/webhook"})
	err = source.Open(context.Background())
	if err == nil {
		source.Close()
		t.Fatal("Open on a port held by another listener returned nil")
	}
	if !strings.Contains(err.Error(), port) {
		t.Errorf("Open error %q does not name the port %s", err, port)
	}
}

// TestWebhookSource_StuckHandlerDoesNotOutliveTheSession covers the shutdown
// escalation. A handler that is still running when the grace period expires
// would otherwise keep publishing after Run returned — into a pipeline the
// manager may already be retiring, on a stream it may already have removed. The
// session has to end anyway, with the connections forced closed.
func TestWebhookSource_StuckHandlerDoesNotOutliveTheSession(t *testing.T) {
	source := NewWebhookSource("stuck-handler", "test.topic", &domain.WebhookConfig{
		Port: "0",
		Path: "/hook",
	})
	source.shutdownGrace = 100 * time.Millisecond

	if err := source.Open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	addr := source.Addr().String()

	// The handler blocks until the test releases it, which is what a publish
	// wedged on an unreachable broker looks like from here.
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	runReturned := make(chan error, 1)
	go func() {
		runReturned <- source.Run(ctx, func(string, domain.StreamEvent) error {
			close(entered)
			<-release
			return nil
		}, func() { close(ready) })
	}()

	<-ready

	postDone := make(chan error, 1)
	go func() {
		res, err := http.Post("http://"+addr+"/hook", "application/json", strings.NewReader(`{"a":1}`))
		if err == nil {
			_ = res.Body.Close()
		}
		postDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}

	// Stop the session while the handler is still inside publish.
	cancel()

	select {
	case err := <-runReturned:
		if err != nil {
			t.Errorf("Run after cancellation returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return: a stuck handler held the session open past its grace period")
	}

	// The in-flight connection has to be gone too. Graceful shutdown alone
	// leaves it open for as long as the handler runs, which is what the
	// escalation to Close exists to bound.
	select {
	case err := <-postDone:
		if err == nil {
			t.Error("the in-flight request completed normally; its connection should have been closed with the session")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight connection outlived the session: shutdown did not escalate to closing it")
	}

	// The listener has to be released, or the port could not be rebound by a
	// restart.
	source.Close()
	probe, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port was not released after the session ended: %v", err)
	}
	_ = probe.Close()
}

// TestWebhookSource_LostListenerEndsTheSession covers the failure the restart
// policy exists for: the listener goes away underneath a running server, so
// Serve returns an error and the session ends with a reason rather than
// quietly continuing to report itself alive.
func TestWebhookSource_LostListenerEndsTheSession(t *testing.T) {
	source := NewWebhookSource("lost-listener", "test.topic", &domain.WebhookConfig{
		Port: "0",
		Path: "/hook",
	})
	if err := source.Open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer source.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	runReturned := make(chan error, 1)
	go func() {
		runReturned <- source.Run(ctx, func(string, domain.StreamEvent) error { return nil }, func() { close(ready) })
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("source never reported ready")
	}

	// Take the listener away, the way a lost bind would.
	if err := source.listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	select {
	case err := <-runReturned:
		if err == nil {
			t.Error("Run returned nil after its listener was closed; the session ended with no reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its listener was closed")
	}
}

// TestWebhookSource_RunWithoutOpen guards the ordering the manager relies on.
func TestWebhookSource_RunWithoutOpen(t *testing.T) {
	source := NewWebhookSource("no-open", "test.topic", nil)
	err := source.Run(context.Background(), func(string, domain.StreamEvent) error { return nil }, func() {})
	if err == nil {
		t.Fatal("Run without Open returned nil")
	}
}
