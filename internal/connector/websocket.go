package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"

	"nephtys/internal/domain"
)

// WebSocketSource connects to a WebSocket endpoint and emits StreamEvents.
//
// Reconnection is not its business: Run reads one connection and returns when
// that connection ends. The manager's supervisor decides whether, when, and how
// many times to run it again.
type WebSocketSource struct {
	id            string
	url           string
	topic         string
	onConnectSend []string
	logger        *slog.Logger
}

// NewWebSocketSource creates a new WebSocket connector. cfg may be nil.
func NewWebSocketSource(id, url, topic string, cfg *domain.WebsocketConfig) *WebSocketSource {
	var onConnectSend []string
	if cfg != nil {
		onConnectSend = cfg.OnConnectSend
	}
	return &WebSocketSource{
		id:            id,
		url:           url,
		topic:         topic,
		onConnectSend: onConnectSend,
		logger:        slog.With("connector", id),
	}
}

func (w *WebSocketSource) ID() string { return w.id }

// Open acquires nothing: a WebSocket source holds no local resource, and the
// dial belongs to the session rather than to registration.
func (w *WebSocketSource) Open(context.Context) error { return nil }

// Close releases nothing, for the same reason. The connection is owned by Run
// and closed before it returns.
func (w *WebSocketSource) Close() {}

// Run dials the endpoint and reads messages until the connection ends or ctx
// is cancelled.
func (w *WebSocketSource) Run(ctx context.Context, publish PublishFunc, ready ReadyFunc) error {
	w.logger.Info("Connecting", "url", w.url)

	// gorilla/websocket returns the HTTP upgrade response. On success the
	// body is owned by the conn; on failure we must close it ourselves
	// to avoid leaking the underlying connection.
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, w.url, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return fmt.Errorf("dial %s: %w", w.url, err)
	}

	// ReadMessage does not observe ctx, so cancellation has to reach it by
	// closing the connection underneath it.
	sessionOver := make(chan struct{})
	defer close(sessionOver)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-sessionOver:
		}
	}()

	defer func() { _ = conn.Close() }()

	if err := w.sendOnConnect(conn); err != nil {
		return fmt.Errorf("post-connect send: %w", err)
	}

	w.logger.Info("Connected")
	ready()

	if err := w.readLoop(ctx, conn, publish); err != nil && ctx.Err() == nil {
		return fmt.Errorf("connection lost: %w", err)
	}
	return nil
}

// sendOnConnect writes the configured post-connect frames verbatim as text
// messages. It runs after every successful handshake, including reconnects.
func (w *WebSocketSource) sendOnConnect(conn *websocket.Conn) error {
	for _, msg := range w.onConnectSend {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			return err
		}
	}
	return nil
}

func (w *WebSocketSource) readLoop(ctx context.Context, conn *websocket.Conn, publish PublishFunc) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		messageType, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		event := w.eventFromMessage(messageType, message)

		if err := publish(w.topic, event); err != nil {
			w.logger.Error("Publish failed", "error", err)
		}
	}
}

func (w *WebSocketSource) eventFromMessage(messageType int, message []byte) domain.StreamEvent {
	now := time.Now().UnixMilli()
	if messageType == websocket.BinaryMessage {
		return domain.StreamEvent{
			Source:      w.id,
			Type:        "websocket_binary",
			Timestamp:   now,
			ContentType: domain.ContentTypeBinary,
			Data:        message,
		}
	}

	eventType, timestamp, seq := inferWebSocketMetadata(message, now)
	payload := json.RawMessage(message)
	if !json.Valid(message) {
		wrapped, _ := json.Marshal(string(message))
		payload = json.RawMessage(wrapped)
	}

	return domain.StreamEvent{
		Source:    w.id,
		Type:      eventType,
		Timestamp: timestamp,
		Seq:       seq,
		Payload:   payload,
	}
}

func inferWebSocketMetadata(message []byte, fallbackTimestamp int64) (string, int64, int64) {
	eventType := "websocket_message"
	timestamp := fallbackTimestamp
	var seq int64

	decoder := json.NewDecoder(bytes.NewReader(message))
	decoder.UseNumber()

	var obj map[string]any
	if err := decoder.Decode(&obj); err != nil {
		return eventType, timestamp, seq
	}

	if rawType, ok := obj["e"].(string); ok && rawType != "" {
		eventType = rawType
	}

	if eventTime, ok := int64FromAny(obj["E"]); ok && eventTime > 0 {
		timestamp = eventTime
	}

	for _, key := range []string{"seq", "u", "lastUpdateId", "t"} {
		if value, ok := int64FromAny(obj[key]); ok && value > 0 {
			seq = value
			break
		}
	}

	return eventType, timestamp, seq
}

func int64FromAny(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}
