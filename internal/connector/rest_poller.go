package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"nephtys/internal/domain"
)

// RESTPollerSource connects to a REST API periodically and emits StreamEvents.
type RESTPollerSource struct {
	id     string
	url    string
	topic  string
	logger *slog.Logger

	config *domain.RestPollerConfig
	client *http.Client

	mu     sync.RWMutex
	status domain.SourceStatus
	cancel context.CancelFunc
}

// NewRESTPollerSource creates a new REST poller connector.
func NewRESTPollerSource(id, url, topic string, config *domain.RestPollerConfig) *RESTPollerSource {
	if config == nil {
		config = &domain.RestPollerConfig{
			Interval: "1m",
			Method:   "GET",
		}
	}
	if config.Interval == "" {
		config.Interval = "1m"
	}
	if config.Method == "" {
		config.Method = "GET"
	}

	return &RESTPollerSource{
		id:     id,
		url:    url,
		topic:  topic,
		config: config,
		status: domain.StatusIdle,
		logger: slog.With("connector", id),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (r *RESTPollerSource) ID() string { return r.id }

func (r *RESTPollerSource) Status() domain.SourceStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status
}

func (r *RESTPollerSource) setStatus(s domain.SourceStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = s
}

// Start begins the polling loop.
func (r *RESTPollerSource) Start(ctx context.Context, publish PublishFunc) error {
	ctx, r.cancel = context.WithCancel(ctx)

	intervalDuration, err := time.ParseDuration(r.config.Interval)
	if err != nil {
		r.logger.Error("Invalid interval duration", "error", err, "interval", r.config.Interval)
		r.setStatus(domain.StatusError)
		return fmt.Errorf("invalid interval duration %q: %w", r.config.Interval, err)
	}

	r.setStatus(domain.StatusRunning)
	r.logger.Info("Started", "interval", intervalDuration, "url", r.url)

	ticker := time.NewTicker(intervalDuration)
	defer ticker.Stop()

	// Perform initial fetch immediately
	r.poll(ctx, publish)

	for {
		select {
		case <-ctx.Done():
			r.setStatus(domain.StatusStopped)
			r.logger.Info("Stopped")
			return ctx.Err()
		case <-ticker.C:
			r.poll(ctx, publish)
		}
	}
}

func (r *RESTPollerSource) poll(ctx context.Context, publish PublishFunc) {
	req, err := http.NewRequestWithContext(ctx, r.config.Method, r.url, nil)
	if err != nil {
		r.logger.Error("Failed to create request", "error", err)
		return
	}

	for k, v := range r.config.Headers {
		req.Header.Set(k, v)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		// Only log as error if context is not canceled
		if ctx.Err() == nil {
			r.logger.Error("Request failed", "error", err)
		}
		return
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			r.logger.Warn("Failed to close response body", "error", closeErr)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.logger.Error("Unexpected response status", "status", resp.Status)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.logger.Error("Failed to read response body", "error", err)
		return
	}

	if len(body) == 0 {
		return
	}

	// Validate JSON format
	var jsonPayload json.RawMessage
	if err := json.Unmarshal(body, &jsonPayload); err != nil {
		// Wrap non-JSON body into a JSON string safely
		escapedBody, _ := json.Marshal(string(body))
		jsonPayload = json.RawMessage(escapedBody)
	}

	event := domain.StreamEvent{
		Source:    r.id,
		Type:      "rest_poller_response",
		Timestamp: time.Now().UnixMilli(),
		Payload:   jsonPayload,
	}

	if err := publish(r.topic, event); err != nil {
		r.logger.Error("Publish failed", "error", err)
	} else {
		r.logger.Debug("Event published", "topic", r.topic)
	}
}

// Stop cancels the source's context.
func (r *RESTPollerSource) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}
