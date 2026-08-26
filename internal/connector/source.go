// Package connector defines the StreamSource interface and its implementations.
package connector

import (
	"context"

	"nephtys/internal/domain"
)

// PublishFunc is the callback a source uses to emit events to the broker.
type PublishFunc func(topic string, event domain.StreamEvent) error

// ReadyFunc is the callback a source uses to report that its session is live:
// the listener is serving, or the upstream handshake has completed. It is
// called at most once per Run.
type ReadyFunc func()

// StreamSource is the interface every connector must implement.
//
// The lifecycle is three phases, and the split is what lets a caller wait for
// the part that is deterministic without waiting for the part that is not.
// Acquisition is local and fails fast, so registration can report it; the
// session is remote and may take arbitrarily long, so registration does not
// wait for it. Retrying is the manager's job, not the connector's: a source
// runs one session and returns, and the backoff ladder and attempt budget live
// in one place instead of five.
//
// A source also reports no status of its own. Lifecycle state is a fact about
// the supervised stream, which the manager owns and publishes.
type StreamSource interface {
	// Open acquires every local resource the source needs: a bound listener,
	// a parsed interval. It performs no I/O to a remote host, which is what
	// makes it fast enough to call under the manager's lock and deterministic
	// enough to answer a registration request with.
	Open(ctx context.Context) error

	// Run serves or reads one session and returns when it ends. It calls ready
	// once the session is live, and returns nil when ctx is cancelled. It does
	// not retry.
	Run(ctx context.Context, publish PublishFunc, ready ReadyFunc) error

	// Close releases what Open acquired. It is called exactly once per Open
	// that returned nil, after Run has returned, and must tolerate a source
	// whose Run was never called.
	Close()

	// ID returns the unique identifier of this source.
	ID() string
}
