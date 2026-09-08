package connector

import (
	"fmt"
	"os"
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
// closed back to back for as long as it is given, ending on the same goroutine
// accounting the fast tests use. It is skipped unless asked for, because it
// trades a bounded runtime for coverage that only appears over repetition.
//
//	make soak                                   # 30s per connector
//	NEPHTYS_SOAK=1 NEPHTYS_SOAK_DURATION=5m go test -run TestConformanceSoak ./internal/connector/

const defaultSoakDuration = 30 * time.Second

func TestConformanceSoak(t *testing.T) {
	if os.Getenv("NEPHTYS_SOAK") == "" {
		t.Skip("soak run: set NEPHTYS_SOAK=1 to run it (or use `make soak`)")
	}

	duration := defaultSoakDuration
	if raw := os.Getenv("NEPHTYS_SOAK_DURATION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("NEPHTYS_SOAK_DURATION %q is not a duration: %v", raw, err)
		}
		if parsed <= 0 {
			t.Fatalf("NEPHTYS_SOAK_DURATION %q must be positive", raw)
		}
		duration = parsed
	}

	for _, c := range connectorCases {
		t.Run(c.name, func(t *testing.T) {
			baseline := connectorGoroutines()

			deadline := time.Now().Add(duration)
			sessions := 0
			for time.Now().Before(deadline) {
				sessions++
				// Each session is its own subtest so its fixture is torn down
				// when it ends, rather than accumulating for the whole run —
				// which would leak servers of the harness's own making and
				// bury the leak being looked for.
				ok := t.Run(fmt.Sprintf("session-%d", sessions), func(t *testing.T) {
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
					t.Fatalf("session %d failed after %d clean ones", sessions, sessions-1)
				}
			}

			t.Logf("%d session(s) in %s", sessions, duration)
			assertGoroutinesSettle(t, baseline)
		})
	}
}
