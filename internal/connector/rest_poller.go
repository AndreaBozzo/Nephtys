package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"nephtys/internal/domain"
)

// RESTPollerSource connects to a REST API periodically and emits StreamEvents.
//
// A poll that fails is not a session failure: the next tick retries, the same
// way it always has. The source therefore has no restart of its own to ask for
// — its session ends only when the stream is stopped.
//
// It reports itself ready after the first poll that reaches the endpoint, not
// on entry. A poller that has never got an answer out of its URL is connecting,
// not running: claiming otherwise would report a stream as healthy while it has
// ingested nothing and never could.
type RESTPollerSource struct {
	id     string
	url    string
	topic  string
	logger *slog.Logger

	config *domain.RestPollerConfig
	client *http.Client

	interval time.Duration
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
		logger: slog.With("connector", id),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (r *RESTPollerSource) ID() string { return r.id }

// Open parses the polling interval. It is the one local resource this source
// needs, and an unparseable interval fails registration rather than a
// goroutine.
func (r *RESTPollerSource) Open(context.Context) error {
	interval, err := time.ParseDuration(r.config.Interval)
	if err != nil {
		return fmt.Errorf("invalid interval duration %q: %w", r.config.Interval, err)
	}
	if interval <= 0 {
		return fmt.Errorf("invalid interval duration %q: must be positive", r.config.Interval)
	}
	r.interval = interval
	return nil
}

// Close releases nothing: the ticker is owned by Run.
func (r *RESTPollerSource) Close() {}

// Run polls the endpoint until ctx is cancelled.
func (r *RESTPollerSource) Run(ctx context.Context, publish PublishFunc, ready ReadyFunc) error {
	r.logger.Info("Started", "interval", r.interval, "url", r.url)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	reported := false
	reachedEndpoint := func(err error) {
		if err != nil || reported {
			return
		}
		reported = true
		ready()
	}

	// Perform initial fetch immediately
	reachedEndpoint(r.poll(ctx, publish))

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("Stopped")
			return nil
		case <-ticker.C:
			reachedEndpoint(r.poll(ctx, publish))
		}
	}
}

// poll performs one request. It returns nil when the endpoint answered, which
// is what the session's readiness is derived from — including an answer that
// carried nothing to publish, since an empty 200 still proves the URL is live.
func (r *RESTPollerSource) poll(ctx context.Context, publish PublishFunc) error {
	req, err := http.NewRequestWithContext(ctx, r.config.Method, r.url, nil)
	if err != nil {
		r.logger.Error("Failed to create request", "error", err)
		return fmt.Errorf("build request: %w", err)
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
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			r.logger.Warn("Failed to close response body", "error", closeErr)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.logger.Error("Unexpected response status", "status", resp.Status)
		return fmt.Errorf("unexpected response status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.logger.Error("Failed to read response body", "error", err)
		return fmt.Errorf("read response body: %w", err)
	}

	if len(body) == 0 {
		return nil
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
	// A publish failure is a pipeline or broker problem, not evidence that the
	// endpoint is unreachable, so it does not change what the poll proved.
	return nil
}
