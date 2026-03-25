package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"nephtys/internal/domain"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// startWSServer creates an httptest server that upgrades to WebSocket,
// sends the provided messages, then closes.
func startWSServer(t *testing.T, messages []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for _, msg := range messages {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				return
			}
		}

		// Keep the connection open until client disconnects
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestWebSocket_IDAndStatus(t *testing.T) {
	src := NewWebSocketSource("ws-id", "wss://x", "t")
	if src.ID() != "ws-id" {
		t.Errorf("expected ws-id, got %s", src.ID())
	}
	if src.Status() != domain.StatusIdle {
		t.Errorf("expected idle, got %s", src.Status())
	}
}

func TestWebSocket_ReceivesMessages(t *testing.T) {
	messages := []string{`{"price":100}`, `{"price":200}`}
	srv := startWSServer(t, messages)

	source := NewWebSocketSource("ws-test", wsURL(srv.URL), "test.topic")

	received := make(chan domain.StreamEvent, 10)
	publish := PublishFunc(func(topic string, event domain.StreamEvent) error {
		received <- event
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	// Wait for both messages
	for i := 0; i < 2; i++ {
		select {
		case evt := <-received:
			var payload map[string]interface{}
			if err := json.Unmarshal(evt.Payload, &payload); err != nil {
				t.Errorf("unmarshal payload: %v", err)
			}
			if evt.Source != "ws-test" {
				t.Errorf("expected source ws-test, got %s", evt.Source)
			}
			if evt.Type != "websocket_message" {
				t.Errorf("expected type websocket_message, got %s", evt.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message %d", i+1)
		}
	}

	cancel()
	source.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for source to stop")
	}
}

func TestWebSocket_Stop(t *testing.T) {
	// Server that stays open
	srv := startWSServer(t, nil)

	source := NewWebSocketSource("ws-stop", wsURL(srv.URL), "test.topic")

	publish := PublishFunc(func(topic string, event domain.StreamEvent) error {
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	// Wait for connection to be established
	deadline := time.After(2 * time.Second)
	for source.Status() != domain.StatusRunning {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for running status")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Stop via cancel
	cancel()
	source.Stop()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stop")
	}

	if source.Status() != domain.StatusStopped {
		t.Errorf("expected stopped status, got %s", source.Status())
	}
}
