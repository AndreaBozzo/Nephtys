// Package broker wraps the NATS messaging infrastructure.
package broker

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"nephtys/internal/domain"
)

// Broker manages the connection to the NATS server.
type Broker struct {
	conn   *nats.Conn
	logger *slog.Logger
}

// Connect establishes a connection to the NATS server at the given URL.
func Connect(url string) (*Broker, error) {
	nc, err := nats.Connect(url)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	slog.Info("Connected to NATS", "url", url)
	return &Broker{conn: nc, logger: slog.With("component", "broker")}, nil
}

// Publish serializes and publishes a StreamEvent to the given NATS subject.
func (b *Broker) Publish(topic string, event domain.StreamEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return b.conn.Publish(topic, data)
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
