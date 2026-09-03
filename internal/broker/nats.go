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

// Reconnect pacing. The jitter is applied on both the plain and TLS paths so
// several Nephtys instances losing one broker do not retry in lockstep.
const (
	reconnectWait   = 2 * time.Second
	reconnectJitter = 500 * time.Millisecond
)

// jetStreamProbeTimeout bounds the round trip JetStreamAvailable makes. It is
// short because a readiness probe that hangs has already failed: the caller
// gets to decide what "not ready" means, and waiting is not one of the options.
const jetStreamProbeTimeout = 2 * time.Second

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
//
// The connection is configured to reconnect indefinitely. The client default is
// 60 attempts, after which it closes the connection for good: a broker outage
// longer than roughly two minutes would leave a process that can never publish
// again and can only be fixed by restarting it. Readiness (`/readyz`) reports
// the reconnect state, so an outage takes the instance out of rotation and
// recovery puts it back without a restart.
func Connect(url string, cfg Config) (*Broker, error) {
	nc, err := nats.Connect(url,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(reconnectWait),
		nats.ReconnectJitter(reconnectJitter, reconnectJitter),
	)
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

// ConnState reports the connection's state as one of the NATS client's own
// status names. The set is closed and is exactly what nats.Status.String()
// can return: CONNECTED, CONNECTING, RECONNECTING, DISCONNECTED, CLOSED,
// DRAINING_SUBS, DRAINING_PUBS, and "unknown status" for a value the client
// itself does not recognize. Nothing of the operator's appears in it, which is
// what makes it safe to serve from an endpoint that carries no auth: the broker
// URL routinely holds credentials, and a NATS error quotes the URL.
func (b *Broker) ConnState() string {
	return b.conn.Status().String()
}

// JetStreamAvailable reports whether JetStream answers on this connection.
//
// It is a separate question from IsConnected, and readiness needs both: a NATS
// server can be connected and serving core NATS while JetStream is disabled,
// unprovisioned for the account, or has lost quorum. Every write Nephtys makes
// goes through JetStream — stream configs to the KV bucket, events to the
// stream — so a connection without it is not an instance that can accept work.
//
// This costs one request/reply round trip to the broker, bounded by
// jetStreamProbeTimeout, so it is only worth asking on a probe and only when
// the connection is up. The error is deliberately discarded rather than
// reported: it can quote the broker URL, and a probe response is public.
func (b *Broker) JetStreamAvailable() bool {
	_, err := b.js.AccountInfo(nats.MaxWait(jetStreamProbeTimeout))
	return err == nil
}

// Close drains and closes the NATS connection.
func (b *Broker) Close() {
	if err := b.conn.Drain(); err != nil {
		b.logger.Warn("NATS drain error", "error", err)
	}
	b.logger.Info("NATS connection closed")
}
