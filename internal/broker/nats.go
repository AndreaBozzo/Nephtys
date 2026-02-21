// Package broker wraps the NATS messaging infrastructure with JetStream support.
package broker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"nephtys/internal/domain"
)

// Config holds JetStream-specific settings.
type Config struct {
	// StreamMaxAge is the maximum age of events before NATS discards them.
	// Zero means no expiration (keep forever until storage limit).
	StreamMaxAge time.Duration

	// StreamMaxBytes is the maximum total size per stream. 0 = unlimited.
	StreamMaxBytes int64
}

// DefaultConfig returns sensible defaults for JetStream.
func DefaultConfig() Config {
	return Config{
		StreamMaxAge:   72 * time.Hour, // Keep events for 3 days
		StreamMaxBytes: 0,              // No size limit
	}
}

// Broker manages the connection to the NATS server with JetStream.
type Broker struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	config Config
	logger *slog.Logger
}

// Connect establishes a connection to the NATS server and initializes JetStream.
func Connect(url string, cfg Config) (*Broker, error) {
	nc, err := nats.Connect(url)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream init: %w", err)
	}

	slog.Info("Connected to NATS with JetStream", "url", url)
	return &Broker{
		conn:   nc,
		js:     js,
		config: cfg,
		logger: slog.With("component", "broker"),
	}, nil
}

// EnsureStream creates or updates a JetStream stream for a given topic prefix.
// The stream captures all subjects matching "<prefix>.>" for durable persistence.
func (b *Broker) EnsureStream(name string, subjects []string) error {
	streamCfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: nats.LimitsPolicy,
		Storage:   nats.FileStorage,
		MaxAge:    b.config.StreamMaxAge,
		MaxBytes:  b.config.StreamMaxBytes,
	}

	// AddStream is idempotent — creates if missing, updates if exists
	_, err := b.js.AddStream(streamCfg)
	if err != nil {
		return fmt.Errorf("ensure stream %q: %w", name, err)
	}

	b.logger.Info("JetStream stream ready", "name", name, "subjects", subjects)
	return nil
}

// Publish serializes and publishes a StreamEvent to JetStream.
// Events are durably stored according to the stream's retention policy.
func (b *Broker) Publish(topic string, event domain.StreamEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = b.js.Publish(topic, data)
	return err
}

// JetStream returns the underlying JetStream context for advanced use
// (KV stores, consumers, etc.).
func (b *Broker) JetStream() nats.JetStreamContext {
	return b.js
}

// IsConnected returns true if the NATS connection is active.
func (b *Broker) IsConnected() bool {
	return b.conn.IsConnected()
}

// Close drains and closes the NATS connection.
func (b *Broker) Close() {
	b.conn.Drain()
	b.logger.Info("NATS connection closed")
}
