package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"nephtys/internal/domain"
)

// SSESource connects to an SSE endpoint and emits StreamEvents.
type SSESource struct {
	id     string
	url    string
	topic  string
	config *domain.SseConfig
	logger *slog.Logger

	mu     sync.RWMutex
	status domain.SourceStatus
	cancel context.CancelFunc
	client *http.Client
}

// NewSSESource creates a new SSE connector.
func NewSSESource(id, url, topic string, config *domain.SseConfig) *SSESource {
	return &SSESource{
		id:     id,
		url:    url,
		topic:  topic,
		config: config,
		status: domain.StatusIdle,
		logger: slog.With("connector", id, "kind", "sse"),
		client: &http.Client{},
	}
}

func (s *SSESource) ID() string { return s.id }

func (s *SSESource) Status() domain.SourceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *SSESource) setStatus(st domain.SourceStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

// Start connects to the SSE endpoint and reads events in a loop.
// It reconnects with exponential backoff on transient errors.
func (s *SSESource) Start(ctx context.Context, publish PublishFunc) error {
	ctx, s.cancel = context.WithCancel(ctx)
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			s.setStatus(domain.StatusStopped)
			s.logger.Info("Stopped")
			return ctx.Err()
		default:
		}

		if attempt > 0 {
			s.setStatus(domain.StatusReconnecting)
			backoff := time.Duration(math.Min(
				float64(initialBackoff)*math.Pow(backoffFactor, float64(attempt-1)),
				float64(maxBackoff),
			))
			s.logger.Info("Reconnecting", "attempt", attempt, "backoff", backoff)

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				s.setStatus(domain.StatusStopped)
				return ctx.Err()
			}
		}

		s.setStatus(domain.StatusConnecting)
		s.logger.Info("Connecting", "url", s.url)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
		if err != nil {
			s.logger.Error("Failed to create request", "error", err)
			s.setStatus(domain.StatusError)
			attempt++
			continue
		}

		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Connection", "keep-alive")

		if s.config != nil && s.config.Headers != nil {
			for k, v := range s.config.Headers {
				req.Header.Set(k, v)
			}
		}

		resp, err := s.client.Do(req)
		if err != nil {
			s.logger.Error("Connection failed", "error", err)
			s.setStatus(domain.StatusError)
			attempt++
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			s.logger.Error("Unexpected status code", "status", resp.StatusCode)
			s.setStatus(domain.StatusError)
			if err := resp.Body.Close(); err != nil {
				s.logger.Warn("Failed to close response body", "error", err)
			}
			attempt++
			continue
		}

		s.setStatus(domain.StatusRunning)
		s.logger.Info("Connected")
		attempt = 0 // reset on successful connection

		err = s.readLoop(ctx, resp, publish)
		if closeErr := resp.Body.Close(); closeErr != nil {
			s.logger.Warn("Failed to close response body", "error", closeErr)
		}

		if ctx.Err() != nil {
			s.setStatus(domain.StatusStopped)
			return ctx.Err()
		}

		s.logger.Warn("Connection lost, will retry", "error", err)
		s.setStatus(domain.StatusError)
		attempt++
	}
}

func (s *SSESource) readLoop(ctx context.Context, resp *http.Response, publish PublishFunc) error {
	scanner := bufio.NewScanner(resp.Body)
	// SSE payloads can be large, increase buffer size if needed, but default is usually fine

	var currentEvent string
	var currentData bytes.Buffer

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()

		// Empty line means end of the event
		if line == "" {
			if currentData.Len() > 0 {
				payloadCopy := make([]byte, currentData.Len())
				copy(payloadCopy, currentData.Bytes())
				payloadData := payloadCopy

				// If strictly not valid JSON, we might want to wrap it in a string or map,
				// but many SSE APIs send JSON in the data field.
				// Let's ensure it's valid JSON for our StreamEvent payload.
				if !json.Valid(payloadData) {
					// Wrap raw string in JSON
					wrapped, _ := json.Marshal(map[string]string{"data": string(payloadData)})
					payloadData = wrapped
				}

				eventType := currentEvent
				if eventType == "" {
					eventType = "sse_message"
				}

				event := domain.StreamEvent{
					Source:    s.id,
					Type:      eventType,
					Timestamp: time.Now().UnixMilli(),
					Payload:   json.RawMessage(payloadData),
				}

				if err := publish(s.topic, event); err != nil {
					s.logger.Error("Publish failed", "error", err)
				}

				// Reset for next event
				currentData.Reset()
				currentEvent = ""
			}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			// Append data (could be multiple lines)
			dataContent := strings.TrimPrefix(line, "data:")
			// The spec says remove a single leading space if present
			if len(dataContent) > 0 && dataContent[0] == ' ' {
				dataContent = dataContent[1:]
			}
			// Append with newline if there's already data
			if currentData.Len() > 0 {
				currentData.WriteString("\n")
			}
			currentData.WriteString(dataContent)
		}
	}

	return scanner.Err()
}

// Stop cancels the context which will terminate the connection and the loop.
func (s *SSESource) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.setStatus(domain.StatusStopped)
}
