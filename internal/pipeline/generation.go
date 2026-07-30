package pipeline

import (
	"context"
	"sync"

	"nephtys/internal/domain"
)

// Generation is one built pipeline together with the lifecycle that retires it.
//
// A stream's pipeline is replaced by building a new generation, pointing
// publishers at it, and retiring the previous one. What makes that a type
// rather than a bare context is ordering. Publishers reach a generation through
// an atomic pointer, so when retirement begins an arbitrary number of them may
// already hold the outgoing handler and be about to call it. Cancelling a
// context tells the buffering middlewares to drain, but says nothing about
// whether a publisher is still on its way in — and an event that lands in a
// buffer after its worker has drained is lost with no error and no metric.
//
// Retire closes that by ordering the two against each other explicitly; see the
// handshake documented there. The cost is one read-lock pair per event on the
// publish path, which is what buys the guarantee that a drain finding an empty
// buffer really means the buffer is empty.
type Generation struct {
	handler Handler

	ctx    context.Context
	cancel context.CancelFunc

	// inFlight is read-held for the duration of every Publish call. Retirement
	// acquires it for writing, which it can only do once every publisher has
	// left the chain.
	inFlight sync.RWMutex

	// sealed is closed once no publisher can reach a buffering middleware ever
	// again. Workers wait for it before their final drain.
	sealed chan struct{}

	// workers holds one channel per buffering middleware, closed when that
	// middleware's goroutine has drained, flushed, and exited. It is appended
	// to only while the generation is being built — before any publisher can
	// see it — so it needs no lock of its own.
	workers []chan struct{}

	retireOnce sync.Once
}

// NewGeneration builds the middleware chain described by cfg, wires publish in
// as its terminal handler, and returns a generation ready to accept events.
//
// The caller owns retirement: every generation that is successfully built must
// eventually be passed to Retire, including one that is abandoned without ever
// being installed, or its buffering middlewares keep their goroutines and their
// buffered events forever.
func NewGeneration(streamID string, cfg *domain.PipelineConfig, publish Handler) (*Generation, error) {
	g := newGeneration()

	pipe, err := buildFromConfig(g, streamID, cfg)
	if err != nil {
		g.Retire()
		return nil, err
	}

	// Execute applies each middleware exactly once, which is when a buffering
	// middleware registers its worker. That happens here, before the generation
	// is returned and therefore before anything can publish through it.
	g.handler = pipe.Execute(publish)
	return g, nil
}

// Publish sends one event through this generation's middleware chain.
//
// The read lock is the hot-path cost of the retirement contract: holding it for
// the duration of the call is what lets Retire establish that no publisher is
// still inside the chain. It is uncontended except during a retirement, where
// the writer is held only long enough to observe that the readers have left.
func (g *Generation) Publish(topic string, event domain.StreamEvent) error {
	g.inFlight.RLock()
	defer g.inFlight.RUnlock()
	return g.handler(topic, event)
}

// Retire shuts this generation down without losing an event it accepted. It
// returns once every such event has been handed downstream, so a caller that
// has already installed a replacement knows the handover is complete.
//
// The handshake is three steps, and the order is the whole point:
//
//  1. Cancel. Publishers still to arrive take the cancelled-context path and
//     forward their event downstream unbatched instead of buffering it, and any
//     publisher parked on a full buffer is released the same way. From here on
//     no *new* event can enter a buffer.
//
//  2. Take the write lock and seal. The lock is only available once every
//     Publish call that was already inside the chain has returned, so sealing
//     under it records the exact moment at which no writer to any buffer
//     remains. Publishers cannot keep arriving indefinitely because callers
//     install the replacement generation before retiring this one, so the
//     population being waited on is finite and each member is on a path that no
//     longer blocks.
//
//  3. Drain. Sealing releases the workers to empty their buffers and exit, and
//     Retire waits for them — which is why a drain that finds a buffer empty is
//     now proof that it is empty rather than merely empty at that instant.
//
// Retire is idempotent and safe to call from multiple goroutines.
func (g *Generation) Retire() {
	g.retireOnce.Do(func() {
		g.cancel()

		// Sealing happens under the write lock, at the one moment the chain is
		// known to be empty. Doing it after releasing would reopen the question
		// of whether a publisher slipped back in.
		g.inFlight.Lock()
		close(g.sealed)
		g.inFlight.Unlock()

		for _, done := range g.workers {
			<-done
		}
	})
}

// newGeneration creates the lifecycle half of a generation, with no chain
// attached. NewGeneration builds on it; tests exercising a single buffering
// middleware use it directly.
func newGeneration() *Generation {
	ctx, cancel := context.WithCancel(context.Background())
	return &Generation{
		ctx:    ctx,
		cancel: cancel,
		sealed: make(chan struct{}),
	}
}

// newWorker registers a buffering middleware's goroutine with the generation
// and returns the channel it must close on exit. Called during build only.
func (g *Generation) newWorker() chan struct{} {
	done := make(chan struct{})
	g.workers = append(g.workers, done)
	return done
}

// Done reports when this generation has stopped accepting events into buffers.
// It is the signal a buffering middleware selects on to begin winding down.
func (g *Generation) Done() <-chan struct{} { return g.ctx.Done() }

// Err mirrors context.Context.Err for this generation.
func (g *Generation) Err() error { return g.ctx.Err() }

// Sealed is closed when no publisher can reach a buffering middleware again. A
// worker that has seen Done must wait for this before its final drain: until
// then, publishers that entered the chain before cancellation may still be
// writing into its buffer.
func (g *Generation) Sealed() <-chan struct{} { return g.sealed }
