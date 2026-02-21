// Package domain defines the core data structures shared across Nephtys.
package domain

import "encoding/json"

// StreamEvent is the standard envelope Nephtys publishes to the broker.
type StreamEvent struct {
	Source    string          `json:"source"`
	Type     string          `json:"type"`
	Timestamp int64          `json:"timestamp"`
	Payload  json.RawMessage `json:"payload"`
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
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`               // "websocket", "sse", "webhook", "grpc"
	URL      string            `json:"url"`
	Topic    string            `json:"topic"`
	Metadata map[string]string `json:"metadata,omitempty"`
}
