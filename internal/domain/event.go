// Package domain defines the core data structures shared across Nephtys.
package domain

import "encoding/json"

// StreamEvent is the standard envelope Nephtys publishes to the broker.
type StreamEvent struct {
	Source    string          `json:"source"`
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// SourceStatus represents the current state of a stream source.
type SourceStatus string

const (
	StatusIdle         SourceStatus = "idle"
	StatusConnecting   SourceStatus = "connecting"
	StatusRunning      SourceStatus = "running"
	StatusReconnecting SourceStatus = "reconnecting"
	StatusStopped      SourceStatus = "stopped"
	StatusError        SourceStatus = "error"
)

// StreamSourceConfig describes how to create and manage a stream source.
type StreamSourceConfig struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"` // "websocket", "sse", "webhook", "grpc", "rest_poller"
	URL        string            `json:"url,omitempty"`
	Topic      string            `json:"topic"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	RestPoller *RestPollerConfig `json:"rest_poller,omitempty"`
	Webhook    *WebhookConfig    `json:"webhook,omitempty"`
	Grpc       *GrpcConfig       `json:"grpc,omitempty"`
	Sse        *SseConfig        `json:"sse,omitempty"`
	Pipeline   *PipelineConfig   `json:"pipeline,omitempty"`
}

// SseConfig configures a Server-Sent Events source.
type SseConfig struct {
	Headers map[string]string `json:"headers,omitempty"` // Custom HTTP headers
}

// GrpcConfig configures a gRPC streaming source.
type GrpcConfig struct {
	Port string `json:"port"` // Port to listen on, e.g., "50051"
}

// RestPollerConfig configures a REST poller source.
type RestPollerConfig struct {
	Interval string            `json:"interval"`         // Polling interval, e.g., "5s", "1m"
	Method   string            `json:"method,omitempty"` // HTTP method, e.g., "GET", "POST". Defaults to "GET".
	Headers  map[string]string `json:"headers,omitempty"`
}

// WebhookConfig configures a webhook receiver source.
type WebhookConfig struct {
	Port      string `json:"port"`                 // Port to listen on, e.g., "8081"
	Path      string `json:"path"`                 // Endpoint path, e.g., "/webhook"
	AuthToken string `json:"auth_token,omitempty"` // Simple token to verify incoming requests
}

// PipelineConfig contains the per-stream middleware configurations.
type PipelineConfig struct {
	Filter    *FilterConfig    `json:"filter,omitempty"`
	Transform *TransformConfig `json:"transform,omitempty"`
	Enrich    *EnrichConfig    `json:"enrich,omitempty"`
	Dedup     *DedupConfig     `json:"dedup,omitempty"`
	Threshold *ThresholdConfig `json:"threshold,omitempty"`
	Batch     *BatchConfig     `json:"batch,omitempty"`
}

// ThresholdConfig configures the threshold/delta anomaly filtering.
type ThresholdConfig struct {
	Enabled bool    `json:"enabled"`
	Path    string  `json:"path"`            // JSON path to the numerical value
	Delta   float64 `json:"delta,omitempty"` // Minimum absolute change required to pass the filter
}

// BatchConfig configures the event batching middleware.
type BatchConfig struct {
	Enabled       bool   `json:"enabled"`
	MaxBatchSize  int    `json:"max_batch_size,omitempty"` // Number of events to batch before flushing (default 100)
	FlushInterval string `json:"flush_interval,omitempty"` // Time interval to flush (default "1s")
}

// FilterConfig allows dropping events that don't match criteria.
type FilterConfig struct {
	// Drop events where Type does not match (exact match if provided)
	MatchTypes []string `json:"match_types,omitempty"`
}

// TransformConfig allows remapping JSON payload fields using dot-notation paths.
type TransformConfig struct {
	// Mapping maps a "new_key" to a "path.to.old_value".
	Mapping map[string]string `json:"mapping,omitempty"`
}

// EnrichConfig allows injecting static tags into events.
type EnrichConfig struct {
	Tags map[string]string `json:"tags,omitempty"`
}

// DedupConfig configures the event deduplication middleware.
type DedupConfig struct {
	Enabled   bool   `json:"enabled"`
	CacheSize int    `json:"cache_size,omitempty"` // Size of LRU/Hash cache (default 1000)
	TTL       string `json:"ttl,omitempty"`        // Time to live (default "1m")
}
