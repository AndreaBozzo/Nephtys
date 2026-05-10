package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"nephtys/internal/domain"
)

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	backoffFactor  = 2.0
)

// WebSocketSource connects to a WebSocket endpoint and emits StreamEvents.
type WebSocketSource struct {
	id     string
	url    string
	topic  string
	logger *slog.Logger

	mu     sync.RWMutex
	conn   *websocket.Conn
	status domain.SourceStatus
	cancel context.CancelFunc
}

// NewWebSocketSource creates a new WebSocket connector.
func NewWebSocketSource(id, url, topic string) *WebSocketSource {
	return &WebSocketSource{
		id:     id,
		url:    url,
		topic:  topic,
		status: domain.StatusIdle,
		logger: slog.With("connector", id),
	}
}

func (w *WebSocketSource) ID() string { return w.id }

func (w *WebSocketSource) Status() domain.SourceStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *WebSocketSource) setStatus(s domain.SourceStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = s
}

// Start connects to the WebSocket and reads messages in a loop.
// It reconnects with exponential backoff on transient errors.
// The function blocks until ctx is cancelled.
func (w *WebSocketSource) Start(ctx context.Context, publish PublishFunc) error {
	ctx, w.cancel = context.WithCancel(ctx)
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			w.setStatus(domain.StatusStopped)
			w.logger.Info("Stopped")
			return ctx.Err()
		default:
		}

		if attempt > 0 {
			w.setStatus(domain.StatusReconnecting)
			backoff := time.Duration(math.Min(
				float64(initialBackoff)*math.Pow(backoffFactor, float64(attempt-1)),
				float64(maxBackoff),
			))
			w.logger.Info("Reconnecting", "attempt", attempt, "backoff", backoff)

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				w.setStatus(domain.StatusStopped)
				return ctx.Err()
			}
		}

		w.setStatus(domain.StatusConnecting)
		w.logger.Info("Connecting", "url", w.url)

		// gorilla/websocket returns the HTTP upgrade response. On success the
		// body is owned by the conn; on failure we must close it ourselves
		// to avoid leaking the underlying connection.
		conn, resp, err := websocket.DefaultDialer.DialContext(ctx, w.url, nil)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			w.logger.Error("Connection failed", "error", err)
			w.setStatus(domain.StatusError)
			attempt++
			continue
		}

		w.setStatus(domain.StatusRunning)
		w.logger.Info("Connected")
		attempt = 0 // reset on successful connection

		w.mu.Lock()
		w.conn = conn
		w.mu.Unlock()

		err = w.readLoop(ctx, conn, publish)

		w.mu.Lock()
		w.conn = nil
		w.mu.Unlock()

		_ = conn.Close()

		if ctx.Err() != nil {
			w.setStatus(domain.StatusStopped)
			return ctx.Err()
		}

		w.logger.Warn("Connection lost, will retry", "error", err)
		w.setStatus(domain.StatusError)
		attempt++
	}
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

// Stop cancels the source's context and forces the active connection to close, tearing down the reader goroutine.
func (w *WebSocketSource) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Lock()
	if w.conn != nil {
		_ = w.conn.Close()
	}
	w.mu.Unlock()
}
