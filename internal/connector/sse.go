package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nephtys/internal/domain"
)

// SSESource connects to an SSE endpoint and emits StreamEvents.
//
// Reconnection is not its business: Run reads one response body and returns
// when that response ends. The manager's supervisor decides whether, when, and
// how many times to run it again.
type SSESource struct {
	id     string
	url    string
	topic  string
	config *domain.SseConfig
	logger *slog.Logger
	client *http.Client
}

// NewSSESource creates a new SSE connector.
func NewSSESource(id, url, topic string, config *domain.SseConfig) *SSESource {
	return &SSESource{
		id:     id,
		url:    url,
		topic:  topic,
		config: config,
		logger: slog.With("connector", id, "kind", "sse"),
		client: &http.Client{},
	}
}

func (s *SSESource) ID() string { return s.id }

// Open acquires nothing: an SSE source holds no local resource, and the request
// belongs to the session rather than to registration.
func (s *SSESource) Open(context.Context) error { return nil }

// Close releases nothing, for the same reason. The response body is owned by
// Run and closed before it returns.
func (s *SSESource) Close() {}

// Run issues one request and reads frames until the stream ends or ctx is
// cancelled.
func (s *SSESource) Run(ctx context.Context, publish PublishFunc, ready ReadyFunc) error {
	s.logger.Info("Connecting", "url", s.url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
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
		return fmt.Errorf("connect: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			s.logger.Warn("Failed to close response body", "error", closeErr)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	s.logger.Info("Connected")
	ready()

	if err := s.readLoop(ctx, resp, publish); err != nil && ctx.Err() == nil {
		return fmt.Errorf("stream ended: %w", err)
	}
	return nil
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
