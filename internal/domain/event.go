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
	URL        string            `json:"url"`
	Topic      string            `json:"topic"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	RestPoller *RestPollerConfig `json:"rest_poller,omitempty"`
	Pipeline   *PipelineConfig   `json:"pipeline,omitempty"`
}

// RestPollerConfig configures a REST poller source.
type RestPollerConfig struct {
	Interval string            `json:"interval"`         // Polling interval, e.g., "5s", "1m"
	Method   string            `json:"method,omitempty"` // HTTP method, e.g., "GET", "POST". Defaults to "GET".
	Headers  map[string]string `json:"headers,omitempty"`
}

// PipelineConfig contains the per-stream middleware configurations.
type PipelineConfig struct {
	Filter    *FilterConfig    `json:"filter,omitempty"`
	Transform *TransformConfig `json:"transform,omitempty"`
	Enrich    *EnrichConfig    `json:"enrich,omitempty"`
	Dedup     *DedupConfig     `json:"dedup,omitempty"`
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
