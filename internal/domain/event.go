// Package domain defines the core data structures shared across Nephtys.
package domain

import (
	"encoding/json"
	"fmt"
)

const (
	// ContentTypeJSON is the default StreamEvent envelope encoding.
	ContentTypeJSON = "application/json"

	// ContentTypeArrowStream identifies Apache Arrow IPC stream payloads.
	ContentTypeArrowStream = "application/vnd.apache.arrow.stream"

	// ContentTypeBinary identifies opaque binary payloads.
	ContentTypeBinary = "application/octet-stream"
)

// StreamEvent is the standard envelope Nephtys publishes to the broker.
type StreamEvent struct {
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Timestamp   int64           `json:"timestamp"`
	Seq         int64           `json:"seq,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`

	// Data carries binary payloads such as Arrow IPC. It is published directly
	// when ContentType is not application/json and is intentionally not encoded
	// into the JSON envelope.
	Data []byte `json:"-"`
}

// IsBinary reports whether this event's bytes live on Data rather than inside
// the JSON envelope. It mirrors the encoding rule applied on the publish path,
// so middlewares and the broker agree on where an event's content is.
func (e StreamEvent) IsBinary() bool {
	return e.ContentType != "" && e.ContentType != ContentTypeJSON
}

// Body returns the bytes that identify this event's content: Data for binary
// events, Payload otherwise. Middlewares that hash or inspect content must use
// this rather than reading Payload directly — a binary event leaves Payload
// nil, so reading it yields the same empty value for every such event.
func (e StreamEvent) Body() []byte {
	if e.IsBinary() {
		return e.Data
	}
	return e.Payload
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
	Websocket  *WebsocketConfig  `json:"websocket,omitempty"`
	Pipeline   *PipelineConfig   `json:"pipeline,omitempty"`
	Restart    *RestartConfig    `json:"restart,omitempty"`
}

// RestartConfig bounds how a stream's supervisor recovers from a session that
// ended. Every field is optional; an omitted field takes the default for the
// stream's kind, which reproduces the behaviour that kind had before restart
// policies existed.
type RestartConfig struct {
	// MaxAttempts caps consecutive restarts before the stream is marked
	// failed. Omitted means unlimited; 0 means never restart. A pointer is
	// what separates those two: encoding "unlimited" as the zero value would
	// give an operator who writes 0 to mean "leave it down" the opposite of
	// what they asked for.
	MaxAttempts *int `json:"max_attempts,omitempty"`

	// InitialBackoff is the delay before the first restart, e.g. "1s".
	InitialBackoff string `json:"initial_backoff,omitempty"`

	// MaxBackoff caps the delay the ladder grows to, e.g. "30s".
	MaxBackoff string `json:"max_backoff,omitempty"`

	// Factor multiplies the delay after each attempt. Must be at least 1.
	Factor float64 `json:"factor,omitempty"`

	// ResetAfter is how long a session must stay up before the attempt budget
	// is earned back, e.g. "60s". Resetting on connect instead would let a
	// source that accepts and immediately drops spin forever without ever
	// reaching a terminal state.
	ResetAfter string `json:"reset_after,omitempty"`
}

// StringList is a []string that also accepts a single JSON string,
// so config authors can write "x" or ["x", "y"] interchangeably.
type StringList []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StringList) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = StringList{single}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("must be a string or an array of strings")
	}
	*s = StringList(list)
	return nil
}

// WebsocketConfig configures a WebSocket source.
type WebsocketConfig struct {
	// OnConnectSend frames are sent verbatim as text messages after every
	// successful handshake, including reconnects.
	OnConnectSend StringList `json:"on_connect_send,omitempty"`
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
	GroupBy string  `json:"group_by,omitempty"`
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
