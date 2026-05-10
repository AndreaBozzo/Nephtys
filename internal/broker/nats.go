// Package broker wraps the NATS messaging infrastructure with JetStream support.
package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

	if _, err := b.js.StreamInfo(name); err != nil {
		if !errors.Is(err, nats.ErrStreamNotFound) {
			return fmt.Errorf("inspect stream %q: %w", name, err)
		}
		if _, err := b.js.AddStream(streamCfg); err != nil {
			return fmt.Errorf("create stream %q: %w", name, err)
		}
	} else if _, err := b.js.UpdateStream(streamCfg); err != nil {
		return fmt.Errorf("update stream %q: %w", name, err)
	}

	b.logger.Info("JetStream stream ready", "name", name, "subjects", subjects)
	return nil
}

// Publish serializes and publishes a StreamEvent to JetStream.
// JSON events are published as the standard envelope. Binary events are
// published directly with Content-Type and sequence headers.
func (b *Broker) Publish(topic string, event domain.StreamEvent) error {
	data, contentType, err := encodeEvent(event)
	if err != nil {
		return err
	}

	msg := &nats.Msg{
		Subject: topic,
		Data:    data,
		Header:  nats.Header{},
	}
	if contentType != "" {
		msg.Header.Set("Content-Type", contentType)
	}
	if event.Seq > 0 {
		msg.Header.Set("X-Nephtys-Seq", strconv.FormatInt(event.Seq, 10))
	}

	_, err = b.js.PublishMsg(msg)
	return err
}

func encodeEvent(event domain.StreamEvent) ([]byte, string, error) {
	contentType := event.ContentType
	if contentType == "" {
		contentType = domain.ContentTypeJSON
	}

	if contentType == domain.ContentTypeJSON {
		data, err := json.Marshal(event)
		if err != nil {
			return nil, "", fmt.Errorf("marshal event: %w", err)
		}
		return data, contentType, nil
	}

	if len(event.Data) == 0 {
		return nil, "", fmt.Errorf("binary event %q has empty data", event.Type)
	}

	return event.Data, contentType, nil
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
	if err := b.conn.Drain(); err != nil {
		b.logger.Warn("NATS drain error", "error", err)
	}
	b.logger.Info("NATS connection closed")
}
