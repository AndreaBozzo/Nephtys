package connector

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nephtys/internal/domain"
)

// The shared connector conformance suite.
//
// Every connector implements the same three-phase contract — Open acquires
// local resources, Run serves one session, Close releases — and until now that
// contract was only ever checked one connector at a time. Each of these tests
// states one clause of it and runs it against every implementation, so a new
// connector is finished when it is added to connectorCases and the suite is
// green, rather than when someone remembers which assertions the last one got.
//
// Where a connector genuinely differs, the difference is a field on its case
// with the reason written next to it, not a weaker assertion for everyone. The
// two that exist are both structural: a push connector has no upstream session
// to lose, and a poller's session outlives any individual poll.
//
// Nothing here waits on a fixed duration for something to happen — every wait
// is on a channel with a generous ceiling — so the suite is deterministic in
// CI. The optional soak run is the one exception and is skipped by default.

// connectorCase registers one connector with the suite. Adding a connector
// means adding a case here; there is deliberately no way to opt out of a
// clause, only to declare which of the two structural exceptions applies.
type connectorCase struct {
	name string

	// build returns the connector wired to a live local upstream.
	build func(t *testing.T) connectorFixture

	// detached returns the same connector with no upstream in existence. Open
	// must still succeed on it: Open acquires local resources and performs no
	// remote I/O, which is the rule that lets the manager hold its lock across
	// it. A push connector has no remote upstream at all, so it returns a
	// source on a port nothing holds.
	detached func(t *testing.T) StreamSource

	// sessionIsTheUpstreamConnection marks the connectors whose session *is*
	// one upstream connection: losing it ends the session, with a reason, and
	// exactly once — retrying is the supervisor's job. A push connector serves
	// whatever clients arrive and a poller's session outlives any single poll,
	// so for those three the session survives instead.
	sessionIsTheUpstreamConnection bool
}

var connectorCases = []connectorCase{
	{
		name:                           "websocket",
		build:                          websocketFixture,
		detached:                       websocketDetached,
		sessionIsTheUpstreamConnection: true,
	},
	{
		name:                           "sse",
		build:                          sseFixture,
		detached:                       sseDetached,
		sessionIsTheUpstreamConnection: true,
	},
	{
		name:     "rest_poller",
		build:    restPollerFixture,
		detached: restPollerDetached,
	},
	{
		name:     "webhook",
		build:    webhookFixture,
		detached: webhookDetached,
	},
	{
		name:     "grpc",
		build:    grpcFixture,
		detached: grpcDetached,
	},
}

// waitCeiling bounds every wait in the suite. It is deliberately far longer
// than anything should take: the tests are fast because they wait on channels,
// not because the ceiling is tight, and a tight one is how a suite becomes
// flaky on a loaded CI runner.
const waitCeiling = 10 * time.Second

// settle is how long a "this did not happen" assertion gives it to happen.
const settle = 250 * time.Millisecond

// --- session harness ---------------------------------------------------

// session is one Run, started and observable.
type session struct {
	source StreamSource
	cancel context.CancelFunc
	done   chan error
	events chan domain.StreamEvent

	readyCalls atomic.Int64
}

// publishRule decides what a session's publish call does with an event.
type publishRule func(domain.StreamEvent) error

func publishOK(domain.StreamEvent) error { return nil }

// start opens the source, runs one session, and waits for it to report ready.
func start(t *testing.T, src StreamSource, rule publishRule) *session {
	t.Helper()

	if err := src.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		source: src,
		cancel: cancel,
		done:   make(chan error, 1),
		// Buffered well past what any test emits, so a connector is never
		// backpressured by the harness unless a test asks for it.
		events: make(chan domain.StreamEvent, 256),
	}

	ready := make(chan struct{})
	var readyOnce atomic.Bool
	go func() {
		s.done <- src.Run(ctx, func(_ string, event domain.StreamEvent) error {
			select {
			case s.events <- event:
			default:
			}
			return rule(event)
		}, func() {
			s.readyCalls.Add(1)
			if readyOnce.CompareAndSwap(false, true) {
				close(ready)
			}
		})
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(waitCeiling):
			t.Error("Run did not return after cancellation")
		}
		src.Close()
	})

	select {
	case <-ready:
	case err := <-s.done:
		t.Fatalf("Run returned %v before reporting ready", err)
	case <-time.After(waitCeiling):
		t.Fatal("source never reported ready")
	}
	return s
}

// nextEvent returns the next published event, failing if none arrives.
func (s *session) nextEvent(t *testing.T) domain.StreamEvent {
	t.Helper()
	select {
	case event := <-s.events:
		return event
	case <-time.After(waitCeiling):
		t.Fatal("no event reached publish")
		return domain.StreamEvent{}
	}
}

// stillRunning reports whether Run is still serving after a settling period.
func (s *session) stillRunning(t *testing.T) bool {
	t.Helper()
	select {
	case err := <-s.done:
		// Put it back so the cleanup's receive does not block.
		s.done <- err
		return false
	case <-time.After(settle):
		return true
	}
}

// waitEnded waits for Run to return on its own and yields its error.
func (s *session) waitEnded(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.done:
		s.done <- err
		return err
	case <-time.After(waitCeiling):
		t.Fatal("Run did not return after the upstream went away")
		return nil
	}
}

// --- the contract ------------------------------------------------------

// TestConformance_OpenPerformsNoRemoteIO is the rule the whole admission path
// rests on: Open acquires local resources only, so the manager can hold its
// lock across it and a registration can block on it. A connector that dialled
// in Open would fail here rather than in production against a host that
// happens to be down.
func TestConformance_OpenPerformsNoRemoteIO(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			src := c.detached(t)

			opened := make(chan error, 1)
			go func() { opened <- src.Open(context.Background()) }()

			select {
			case err := <-opened:
				if err != nil {
					t.Fatalf("Open with no upstream in existence returned %v, want nil", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Open did not return within a second — it is reaching a remote host")
			}
			src.Close()
		})
	}
}

// TestConformance_IDIsStable guards the identity every metric series, log line
// and port claim is keyed on.
func TestConformance_IDIsStable(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			before := fix.source.ID()
			if before == "" {
				t.Fatal("ID is empty")
			}

			s := start(t, fix.source, publishOK)
			if during := fix.source.ID(); during != before {
				t.Fatalf("ID changed from %q to %q while running", before, during)
			}
			s.cancel()
			if after := fix.source.ID(); after != before {
				t.Fatalf("ID changed from %q to %q after cancellation", before, after)
			}
		})
	}
}

// TestConformance_ReadyFiresExactlyOnce covers the half of the contract the
// manager uses to move a stream from connecting to running. Firing twice would
// double-count a restart's uptime; never firing leaves a live stream reading as
// connecting forever.
func TestConformance_ReadyFiresExactlyOnce(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			s := start(t, fix.source, publishOK)

			// start already waited for the first call. Give the session
			// something to do, then check nothing fired a second time.
			fix.emit(t)
			s.nextEvent(t)

			if got := s.readyCalls.Load(); got != 1 {
				t.Fatalf("ready called %d times, want exactly 1", got)
			}
		})
	}
}

// TestConformance_CancellationEndsTheSessionWithNil is what makes a stop a stop
// rather than a failure. A source that returned its context error here would
// have the supervisor record a stream shut down on purpose as one that broke.
func TestConformance_CancellationEndsTheSessionWithNil(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			s := start(t, fix.source, publishOK)

			s.cancel()
			select {
			case err := <-s.done:
				s.done <- err
				if err != nil {
					t.Fatalf("Run after cancellation returned %v, want nil", err)
				}
			case <-time.After(waitCeiling):
				t.Fatal("Run did not return after cancellation")
			}
		})
	}
}

// TestConformance_EventsCarryTheirSourceIdentity covers what every consumer,
// metric and dedup series downstream reads off an event.
func TestConformance_EventsCarryTheirSourceIdentity(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			s := start(t, fix.source, publishOK)

			fix.emit(t)
			event := s.nextEvent(t)

			if event.Source != fix.source.ID() {
				t.Errorf("event source %q, want %q", event.Source, fix.source.ID())
			}
			if event.Type == "" {
				t.Error("event has no type; the filter middleware matches on it")
			}
			if event.Timestamp <= 0 {
				t.Errorf("event timestamp is %d", event.Timestamp)
			}
			if len(event.Body()) == 0 {
				t.Error("event carries no body")
			}
		})
	}
}

// TestConformance_MalformedFrameStillProducesAPublishableEvent is the
// fault-injection case with the most reach. A source that hands the broker an
// event it cannot encode fails every publish for as long as the upstream keeps
// sending that shape, and the failure surfaces as a marshalling error a long
// way from the connector that caused it. Wrapping is how the other connectors
// avoid it, and the assertion is the broker's own encoding rule.
func TestConformance_MalformedFrameStillProducesAPublishableEvent(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			s := start(t, fix.source, publishOK)

			fix.emitMalformed(t)

			// A poller may still be delivering well-formed events queued
			// before the switch, so the malformed one is identified by its
			// marker rather than by being the next one to arrive.
			deadline := time.After(waitCeiling)
			for {
				var event domain.StreamEvent
				select {
				case event = <-s.events:
				case <-deadline:
					t.Fatal("no event derived from the malformed frame reached publish")
				}

				assertEncodable(t, event)
				if strings.Contains(string(event.Body()), malformedMarker) {
					return
				}
			}
		})
	}
}

// assertEncodable applies the broker's encoding rule to an event: a JSON event
// is marshalled as the envelope, and anything else is published as raw bytes.
// It mirrors broker.encodeEvent, which is unexported and needs a live NATS
// server to reach through Publish.
func assertEncodable(t *testing.T, event domain.StreamEvent) {
	t.Helper()

	if event.IsBinary() {
		if len(event.Data) == 0 {
			t.Fatalf("binary event %q has empty data; the broker rejects it", event.Type)
		}
		return
	}
	if _, err := json.Marshal(event); err != nil {
		t.Fatalf("the broker cannot encode this event: %v\npayload: %s", err, event.Payload)
	}
}

// TestConformance_PublishFailureDoesNotEndTheSession is dependency loss seen
// from the connector: the broker is unreachable, or a pipeline is rejecting
// everything. Ending the session would spend a restart attempt on a fault a
// restart cannot fix, and would take the stream down for the duration of an
// outage it is supposed to ride out.
func TestConformance_PublishFailureDoesNotEndTheSession(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			s := start(t, fix.source, func(domain.StreamEvent) error {
				return errDependencyLost
			})

			fix.emit(t)
			s.nextEvent(t)

			if !s.stillRunning(t) {
				t.Fatal("Run ended because a publish failed")
			}

			// And it is still ingesting, not merely still running.
			fix.emit(t)
			s.nextEvent(t)
		})
	}
}

// TestConformance_SlowPublishBackpressuresRatherThanDrops states the choice
// every connector makes when the pipeline is slower than the upstream: publish
// is called synchronously and the session waits, so a slow consumer costs
// throughput rather than events. The alternative — a buffer that overflows —
// loses data silently, which is the failure nobody notices.
func TestConformance_SlowPublishBackpressuresRatherThanDrops(t *testing.T) {
	const events = 3

	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			var published atomic.Int64
			s := start(t, fix.source, func(domain.StreamEvent) error {
				time.Sleep(20 * time.Millisecond)
				published.Add(1)
				return nil
			})

			for i := 0; i < events; i++ {
				fix.emit(t)
			}
			for i := 0; i < events; i++ {
				s.nextEvent(t)
			}

			// An event reaches the harness on the way into publish, so the last
			// one is still inside the slow call at this point. What is being
			// asserted is that every publish *completes*, so the count is
			// waited for rather than sampled.
			deadline := time.Now().Add(waitCeiling)
			for published.Load() < events {
				if time.Now().After(deadline) {
					t.Fatalf("%d event(s) finished publishing, want at least %d", published.Load(), events)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// TestConformance_CloseReleasesWhatOpenAcquired is the assertion a restart
// depends on: the supervisor closes a source and opens it again on the same
// address, and a listener that outlived its Close makes the second Open fail
// with the stream's own port as the conflict.
func TestConformance_CloseReleasesWhatOpenAcquired(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)

			if err := fix.source.Open(context.Background()); err != nil {
				t.Fatalf("first Open: %v", err)
			}
			fix.source.Close()

			if err := fix.source.Open(context.Background()); err != nil {
				t.Fatalf("second Open after Close: %v — Close did not release what Open acquired", err)
			}
			fix.source.Close()
		})
	}
}

// TestConformance_CloseIsSafeWithoutRun covers the admission path that fails
// between Open and Run — a config store that rejects the write, a cancelled
// registration — where Close runs against a source whose Run was never called.
func TestConformance_CloseIsSafeWithoutRun(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)

			if err := fix.source.Open(context.Background()); err != nil {
				t.Fatalf("Open: %v", err)
			}
			fix.source.Close()
			// Twice: the push connectors document Close as safe to repeat, and
			// for the others it is a no-op either way.
			fix.source.Close()
		})
	}
}

// TestConformance_NoConnectorGoroutineSurvivesTheSession is the leak check.
// It counts goroutines whose stack runs through this package — the ones a
// connector starts and therefore owns — which is precise enough to catch a
// watcher left parked on a channel or a Serve loop that outlived its listener,
// and immune to the idle-connection noise that makes a whole-process count
// flaky.
func TestConformance_NoConnectorGoroutineSurvivesTheSession(t *testing.T) {
	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			baseline := connectorGoroutines()

			fix := c.build(t)
			s := start(t, fix.source, publishOK)
			fix.emit(t)
			s.nextEvent(t)

			s.cancel()
			select {
			case err := <-s.done:
				s.done <- err
			case <-time.After(waitCeiling):
				t.Fatal("Run did not return after cancellation")
			}
			fix.source.Close()

			assertGoroutinesSettle(t, baseline)
		})
	}
}

// --- upstream loss -----------------------------------------------------

// TestConformance_UpstreamLossEndsAPullSession covers the disconnect fault for
// the connectors whose session is one upstream connection. Three things have to
// hold together: the session ends, it ends with a reason the supervisor can put
// in last_error, and the source does not reconnect on its own — retrying is the
// supervisor's job, and a connector that also retried would spend the stream's
// attempt budget invisibly.
func TestConformance_UpstreamLossEndsAPullSession(t *testing.T) {
	for _, c := range connectorCases {
		if !c.sessionIsTheUpstreamConnection {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			baseline := connectorGoroutines()

			fix := c.build(t)
			s := start(t, fix.source, publishOK)

			fix.loseUpstream(t)

			if err := s.waitEnded(t); err == nil {
				t.Fatal("the session ended with nil after the upstream went away; the supervisor has no reason to record")
			}
			if got := fix.sessions(); got != 1 {
				t.Fatalf("the upstream was reached %d times, want 1 — the source is retrying on its own", got)
			}

			// A session that ends because the far end went away is the failure
			// path a supervised stream spends most of its restarts on, so it is
			// the one that has to be clean: a watcher left parked here leaks
			// once per reconnect, for as long as the upstream keeps flapping.
			fix.source.Close()
			assertGoroutinesSettle(t, baseline)
		})
	}
}

// TestConformance_UpstreamLossDoesNotEndAPushSession is the same fault for the
// connectors that have no upstream connection to lose. A webhook or gRPC source
// serves whoever arrives, and a poller's session outlives any single poll — a
// dead endpoint is retried on the next tick, which is why no restart policy
// applies to it. In all three cases the session has to stay up.
func TestConformance_UpstreamLossDoesNotEndAPushSession(t *testing.T) {
	for _, c := range connectorCases {
		if c.sessionIsTheUpstreamConnection {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			fix := c.build(t)
			s := start(t, fix.source, publishOK)

			fix.loseUpstream(t)

			if !s.stillRunning(t) {
				t.Fatal("the session ended when the upstream went away")
			}
		})
	}
}

// --- goroutine accounting ----------------------------------------------

// connectorGoroutines counts the goroutines currently running through this
// package. The test's own goroutines are in it too, which is why every use
// compares against a baseline taken in the same test rather than against zero.
func connectorGoroutines() int {
	count, _ := connectorStacks()
	return count
}

func connectorStacks() (int, string) {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}

	var count int
	var dump strings.Builder
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(stack, "nephtys/internal/connector.") {
			continue
		}
		count++
		dump.WriteString(stack)
		dump.WriteString("\n\n")
	}
	return count, dump.String()
}

// assertGoroutinesSettle waits for the count to come back to its baseline. It
// polls rather than sampling once because a goroutine that has been told to
// stop is not required to have stopped by the time the call that told it
// returns.
func assertGoroutinesSettle(t *testing.T, baseline int) {
	t.Helper()

	deadline := time.Now().Add(waitCeiling)
	for {
		count, dump := connectorStacks()
		if count <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connector goroutine(s) survived the session, baseline was %d:\n%s", count, baseline, dump)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
