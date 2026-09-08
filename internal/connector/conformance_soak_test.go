package connector

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// The optional soak run.
//
// Everything else in the conformance suite exercises one session per
// assertion, which is the right shape for CI: deterministic, channel-driven,
// over in seconds. What it cannot see is accumulation — a resource released
// once per session but acquired twice, a listener that rebinds ninety-nine
// times out of a hundred — and that is exactly the shape of a supervised
// stream under a flapping upstream, which restarts on a ladder for as long as
// the fault lasts.
//
// This run drives that path directly: sessions opened, served, cancelled and
// closed back to back, ending on the same goroutine accounting the fast tests
// use. It is skipped unless asked for, because it trades runtime for coverage
// that only appears over repetition.
//
//	make soak                                        # 300 sessions per connector
//	NEPHTYS_SOAK=1 NEPHTYS_SOAK_SESSIONS=1000 go test -count=1 -run TestConformanceSoak ./internal/connector/
//
// **It is bounded by session count, not by wall time**, and that is the
// correction of a first attempt that ran for a fixed 30 seconds. Every session
// opens and closes a TCP connection to a local fixture, and a closed socket
// holds its port in TIME_WAIT for minutes afterwards — so a duration-bounded
// run goes as fast as the host allows and eventually exhausts the ephemeral
// port range rather than finding anything. On Windows it did: WSAEADDRINUSE
// after some nine thousand WebSocket sessions, and then a first-session failure
// for every connector that ran afterwards against a port table that had not
// recovered. What that run measured was the host.
//
// A leak that happens once per session is visible within tens of sessions, so
// the default sits well above the signal and well below the ceiling: 300 per
// connector, the whole suite in about three seconds. 5,000 sessions on one
// connector were also measured clean here, which is the headroom the default
// has. Raise NEPHTYS_SOAK_SESSIONS far enough and the port table is what gives
// out first — a property of the machine, not a connector defect.
const (
	defaultSoakSessions = 300

	// soakCeiling stops a run that is going nowhere, whatever the session
	// count says. It is a safety net, not the bound.
	soakCeiling = 10 * time.Minute
)

func TestConformanceSoak(t *testing.T) {
	if os.Getenv("NEPHTYS_SOAK") == "" {
		t.Skip("soak run: set NEPHTYS_SOAK=1 to run it (or use `make soak`)")
	}

	sessionsWanted := defaultSoakSessions
	if raw := os.Getenv("NEPHTYS_SOAK_SESSIONS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("NEPHTYS_SOAK_SESSIONS %q is not a number: %v", raw, err)
		}
		if parsed <= 0 {
			t.Fatalf("NEPHTYS_SOAK_SESSIONS %q must be positive", raw)
		}
		sessionsWanted = parsed
	}

	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			baseline := connectorGoroutines()

			started := time.Now()
			deadline := started.Add(soakCeiling)

			completed := 0
			for completed < sessionsWanted {
				if time.Now().After(deadline) {
					t.Fatalf("soak ceiling of %s reached after %d of %d session(s)", soakCeiling, completed, sessionsWanted)
				}
				completed++

				// Each session is its own subtest so its fixture is torn down
				// when it ends, rather than accumulating for the whole run —
				// which would leak servers of the harness's own making and
				// bury the leak being looked for.
				ok := t.Run(fmt.Sprintf("session-%d", completed), func(t *testing.T) {
					fix := c.build(t)
					s := start(t, fix.source, publishOK)

					fix.emit(t)
					s.nextEvent(t)

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
					fix.source.Close()
				})
				if !ok {
					t.Fatalf("session %d failed after %d clean one(s)", completed, completed-1)
				}
			}

			t.Logf("%d session(s) in %s", completed, time.Since(started).Round(time.Millisecond))
			assertGoroutinesSettle(t, baseline)
		})
	}
}
