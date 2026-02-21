// Package connector defines the StreamSource interface and its implementations.
package connector

import (
	"context"

	"nephtys/internal/domain"
)

// PublishFunc is the callback a source uses to emit events to the broker.
type PublishFunc func(topic string, event domain.StreamEvent) error

// StreamSource is the interface every connector must implement.
type StreamSource interface {
	// Start begins ingesting data. It blocks until the context is cancelled
	// or an unrecoverable error occurs. Events are emitted via publish.
	Start(ctx context.Context, publish PublishFunc) error

	// Stop signals the source to shut down gracefully.
	Stop()

	// ID returns the unique identifier of this source.
	ID() string

	// Status returns the current operational status.
	Status() domain.SourceStatus
}
