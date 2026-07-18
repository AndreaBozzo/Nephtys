package connector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func startWSFrameServer(t *testing.T, frames []struct {
	messageType int
	payload     []byte
}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for _, frame := range frames {
			if err := conn.WriteMessage(frame.messageType, frame.payload); err != nil {
				return
			}
		}

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
	src := NewWebSocketSource("ws-id", "wss://x", "t", nil)
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

	source := NewWebSocketSource("ws-test", wsURL(srv.URL), "test.topic", nil)

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

func TestWebSocket_InferBinanceMetadata(t *testing.T) {
	messages := []string{`{"e":"trade","E":1700000000001,"t":12345,"s":"BTCUSDT","p":"42000"}`}
	srv := startWSServer(t, messages)

	source := NewWebSocketSource("binance_btc", wsURL(srv.URL), "test.topic", nil)

	received := make(chan domain.StreamEvent, 1)
	publish := PublishFunc(func(topic string, event domain.StreamEvent) error {
		received <- event
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	select {
	case evt := <-received:
		if evt.Type != "trade" {
			t.Fatalf("expected Binance event type trade, got %s", evt.Type)
		}
		if evt.Timestamp != 1700000000001 {
			t.Fatalf("expected Binance event timestamp, got %d", evt.Timestamp)
		}
		if evt.Seq != 12345 {
			t.Fatalf("expected trade id as sequence, got %d", evt.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}

	cancel()
	source.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for source to stop")
	}
}

func TestWebSocket_BinaryMessage(t *testing.T) {
	srv := startWSFrameServer(t, []struct {
		messageType int
		payload     []byte
	}{
		{messageType: websocket.BinaryMessage, payload: []byte{0xff, 0x00, 0x01}},
	})

	source := NewWebSocketSource("ws-bin", wsURL(srv.URL), "test.topic", nil)
	received := make(chan domain.StreamEvent, 1)
	publish := PublishFunc(func(topic string, event domain.StreamEvent) error {
		received <- event
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	select {
	case evt := <-received:
		if evt.ContentType != domain.ContentTypeBinary {
			t.Fatalf("expected binary content type, got %q", evt.ContentType)
		}
		if string(evt.Data) != string([]byte{0xff, 0x00, 0x01}) {
			t.Fatalf("unexpected binary payload: %v", evt.Data)
		}
		if len(evt.Payload) != 0 {
			t.Fatalf("expected no JSON payload for binary frame, got %q", evt.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for binary message")
	}

	cancel()
	source.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for source to stop")
	}
}

func TestWebSocket_NonJSONTextIsWrapped(t *testing.T) {
	srv := startWSServer(t, []string{"plain text"})

	source := NewWebSocketSource("ws-text", wsURL(srv.URL), "test.topic", nil)
	received := make(chan domain.StreamEvent, 1)
	publish := PublishFunc(func(topic string, event domain.StreamEvent) error {
		received <- event
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	select {
	case evt := <-received:
		var payload string
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("expected wrapped JSON string payload: %v", err)
		}
		if payload != "plain text" {
			t.Fatalf("unexpected wrapped payload %q", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for text message")
	}

	cancel()
	source.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for source to stop")
	}
}

func TestInferWebSocketMetadata_DepthUpdateUsesExchangeSequence(t *testing.T) {
	message := []byte(`{"e":"depthUpdate","E":1700000000100,"U":100,"u":105,"s":"BTCUSDT"}`)

	eventType, timestamp, seq := inferWebSocketMetadata(message, 123)

	if eventType != "depthUpdate" {
		t.Fatalf("expected depthUpdate, got %s", eventType)
	}
	if timestamp != 1700000000100 {
		t.Fatalf("expected exchange timestamp, got %d", timestamp)
	}
	if seq != 105 {
		t.Fatalf("expected final update id as sequence, got %d", seq)
	}
}

func TestWebSocket_OnConnectSendResentOnReconnect(t *testing.T) {
	frames := []string{`{"action":"auth","token":"x"}`, `{"action":"subscribe","channel":"a"}`}

	// Server reads the two configured frames, reports them, then drops the
	// connection — forcing the client to reconnect and re-send.
	received := make(chan string, 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for i := 0; i < len(frames); i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			received <- string(msg)
		}
	}))
	t.Cleanup(srv.Close)

	source := NewWebSocketSource("ws-sub", wsURL(srv.URL), "test.topic", &domain.WebsocketConfig{
		OnConnectSend: domain.StringList(frames),
	})

	publish := PublishFunc(func(topic string, event domain.StreamEvent) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	// Expect the frames in order on the first connection, then again after the
	// server-initiated disconnect (reconnect backoff starts at 1s).
	for round := 0; round < 2; round++ {
		for i, want := range frames {
			select {
			case got := <-received:
				if got != want {
					t.Fatalf("round %d frame %d: got %q, want %q", round, i, got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for round %d frame %d", round, i)
			}
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

func TestWebSocket_SendOnConnectWriteError(t *testing.T) {
	srv := startWSServer(t, nil)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	src := NewWebSocketSource("ws-err", "wss://x", "t", &domain.WebsocketConfig{
		OnConnectSend: domain.StringList{`{"action":"subscribe"}`},
	})
	if err := src.sendOnConnect(conn); err == nil {
		t.Fatal("expected error writing to closed connection")
	}
}

func TestWebSocket_OnConnectSendRecoversFromAbortedConnection(t *testing.T) {
	frames := []string{`{"action":"subscribe","channel":"a"}`}

	// The first connection is aborted with a TCP RST right after the
	// handshake, so the post-connect send hits a dead connection; the
	// source must retry and deliver the frame on the next connection.
	var connCount atomic.Int32
	received := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if connCount.Add(1) == 1 {
			if tcp, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			received <- string(msg)
		}
	}))
	t.Cleanup(srv.Close)

	source := NewWebSocketSource("ws-abort", wsURL(srv.URL), "test.topic", &domain.WebsocketConfig{
		OnConnectSend: domain.StringList(frames),
	})

	publish := PublishFunc(func(topic string, event domain.StreamEvent) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- source.Start(ctx, publish)
	}()

	select {
	case got := <-received:
		if got != frames[0] {
			t.Fatalf("got %q, want %q", got, frames[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for frame after aborted connection")
	}
	if connCount.Load() < 2 {
		t.Fatalf("expected at least 2 connections, got %d", connCount.Load())
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

	source := NewWebSocketSource("ws-stop", wsURL(srv.URL), "test.topic", nil)

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
