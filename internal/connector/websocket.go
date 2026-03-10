package connector

import (
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

		conn, _, err := websocket.DefaultDialer.DialContext(ctx, w.url, nil)
		if err != nil {
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

		_, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		event := domain.StreamEvent{
			Source:    w.id,
			Type:      "websocket_message",
			Timestamp: time.Now().UnixMilli(),
			Payload:   json.RawMessage(message),
		}

		if err := publish(w.topic, event); err != nil {
			w.logger.Error("Publish failed", "error", err)
		}
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
