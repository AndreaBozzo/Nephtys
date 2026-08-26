package server

import (
	"strings"
	"testing"
	"time"

	"nephtys/internal/domain"
)

// TestRestartPolicyDefaults pins the per-kind defaults. The pull connectors
// keep exactly what they had when they retried internally — forever, on a
// 1s→30s ladder. The push connectors gain a small bounded budget: a lost
// listener used to be terminal on the first failure and needed a human.
func TestRestartPolicyDefaults(t *testing.T) {
	tests := []struct {
		kind        string
		maxAttempts int
	}{
		{"websocket", unlimitedAttempts},
		{"sse", unlimitedAttempts},
		{"rest_poller", unlimitedAttempts},
		{"webhook", defaultPushAttempts},
		{"grpc", defaultPushAttempts},
	}

	for _, tt := range tests {
		policy := restartPolicyFor(domain.StreamSourceConfig{Kind: tt.kind})
		if policy.maxAttempts != tt.maxAttempts {
			t.Errorf("%s max_attempts = %d, want %d", tt.kind, policy.maxAttempts, tt.maxAttempts)
		}
		if policy.initialBackoff != time.Second || policy.maxBackoff != 30*time.Second {
			t.Errorf("%s ladder = %s..%s, want 1s..30s", tt.kind, policy.initialBackoff, policy.maxBackoff)
		}
		if policy.factor != 2 {
			t.Errorf("%s factor = %v, want 2", tt.kind, policy.factor)
		}
		if policy.resetAfter != time.Minute {
			t.Errorf("%s reset_after = %s, want 1m", tt.kind, policy.resetAfter)
		}
	}
}

func TestRestartPolicyOverrides(t *testing.T) {
	attempts := 4
	policy := restartPolicyFor(domain.StreamSourceConfig{
		Kind: "websocket",
		Restart: &domain.RestartConfig{
			MaxAttempts:    &attempts,
			InitialBackoff: "250ms",
			MaxBackoff:     "2s",
			Factor:         3,
			ResetAfter:     "10s",
		},
	})

	if policy.maxAttempts != 4 {
		t.Errorf("max_attempts = %d, want 4", policy.maxAttempts)
	}
	if policy.initialBackoff != 250*time.Millisecond {
		t.Errorf("initial_backoff = %s, want 250ms", policy.initialBackoff)
	}
	if policy.maxBackoff != 2*time.Second {
		t.Errorf("max_backoff = %s, want 2s", policy.maxBackoff)
	}
	if policy.factor != 3 {
		t.Errorf("factor = %v, want 3", policy.factor)
	}
	if policy.resetAfter != 10*time.Second {
		t.Errorf("reset_after = %s, want 10s", policy.resetAfter)
	}
}

// TestRestartPolicyZeroAttemptsMeansNeverRestart is the reason max_attempts is
// a pointer: 0 and "omitted" have to mean opposite things.
func TestRestartPolicyZeroAttemptsMeansNeverRestart(t *testing.T) {
	never := 0
	bounded := restartPolicyFor(domain.StreamSourceConfig{
		Kind:    "websocket",
		Restart: &domain.RestartConfig{MaxAttempts: &never},
	})
	if _, ok := bounded.next(0); ok {
		t.Error("max_attempts 0 allowed a restart")
	}

	unbounded := restartPolicyFor(domain.StreamSourceConfig{
		Kind:    "websocket",
		Restart: &domain.RestartConfig{InitialBackoff: "1s"},
	})
	if _, ok := unbounded.next(1000); !ok {
		t.Error("an omitted max_attempts stopped restarting")
	}
}

func TestRestartPolicyDelayLadder(t *testing.T) {
	policy := restartPolicyFor(domain.StreamSourceConfig{Kind: "websocket"})

	want := []time.Duration{
		time.Second,      // attempt 1
		2 * time.Second,  // attempt 2
		4 * time.Second,  // attempt 3
		8 * time.Second,  // attempt 4
		16 * time.Second, // attempt 5
		30 * time.Second, // attempt 6, capped
		30 * time.Second, // and stays capped
	}
	for i, expected := range want {
		if got := policy.delay(i + 1); got != expected {
			t.Errorf("delay(%d) = %s, want %s", i+1, got, expected)
		}
	}
}

// TestValidateRestartComparesEffectiveLadder covers the case an explicit-only
// comparison misses. "max_backoff": "500ms" alone asks for a cap under the 1s
// default initial backoff; the resolver would have to widen it back to 1s,
// which is a silent reinterpretation of a value the operator wrote.
func TestValidateRestartComparesEffectiveLadder(t *testing.T) {
	err := validateRestart(&domain.RestartConfig{MaxBackoff: "500ms"})
	if err == nil {
		t.Fatal("a max_backoff below the default initial_backoff passed validation")
	}
	if !strings.Contains(err.Error(), "initial_backoff") {
		t.Errorf("error %q does not explain which field it conflicts with", err)
	}

	// The mirror case: an initial above the default max is the same conflict
	// seen from the other side.
	if err := validateRestart(&domain.RestartConfig{InitialBackoff: "5m"}); err == nil {
		t.Error("an initial_backoff above the default max_backoff passed validation")
	}

	// And a ladder that grows is accepted whichever half is written.
	if err := validateRestart(&domain.RestartConfig{MaxBackoff: "5m"}); err != nil {
		t.Errorf("a wider max_backoff was rejected: %v", err)
	}
}

// TestRestartPolicyLadderAlwaysGrows states the invariant the validator now
// guarantees: every config that reaches the resolver has max >= initial.
func TestRestartPolicyLadderAlwaysGrows(t *testing.T) {
	policy := restartPolicyFor(domain.StreamSourceConfig{
		Kind:    "websocket",
		Restart: &domain.RestartConfig{InitialBackoff: "2s", MaxBackoff: "8s"},
	})
	previous := time.Duration(0)
	for attempt := 1; attempt <= 6; attempt++ {
		got := policy.delay(attempt)
		if got < previous {
			t.Fatalf("delay(%d) = %s, shorter than the previous %s", attempt, got, previous)
		}
		if got > 8*time.Second {
			t.Fatalf("delay(%d) = %s, above the configured cap", attempt, got)
		}
		previous = got
	}
}

func TestValidateRestart(t *testing.T) {
	negative, zero := -1, 0

	tests := []struct {
		name    string
		restart *domain.RestartConfig
		wantErr bool
	}{
		{"omitted", nil, false},
		{"empty block", &domain.RestartConfig{}, false},
		{"zero attempts", &domain.RestartConfig{MaxAttempts: &zero}, false},
		{"negative attempts", &domain.RestartConfig{MaxAttempts: &negative}, true},
		{"factor below one", &domain.RestartConfig{Factor: 0.5}, true},
		{"factor of one", &domain.RestartConfig{Factor: 1}, false},
		{"unparseable backoff", &domain.RestartConfig{InitialBackoff: "5 seconds"}, true},
		{"bare number backoff", &domain.RestartConfig{MaxBackoff: "30"}, true},
		{"zero duration", &domain.RestartConfig{ResetAfter: "0s"}, true},
		{"negative duration", &domain.RestartConfig{InitialBackoff: "-1s"}, true},
		{"max below initial", &domain.RestartConfig{InitialBackoff: "10s", MaxBackoff: "1s"}, true},
		{"complete and sound", &domain.RestartConfig{
			MaxAttempts:    &zero,
			InitialBackoff: "1s",
			MaxBackoff:     "30s",
			Factor:         2,
			ResetAfter:     "1m",
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRestart(tt.restart)
			if tt.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateStreamConfigChecksRestart makes sure the block is reached by the
// one validator every entry point shares, rather than only by a direct call.
func TestValidateStreamConfigChecksRestart(t *testing.T) {
	cfg := domain.StreamSourceConfig{
		ID:      "restart-validated",
		Kind:    "webhook",
		Topic:   "nephtys.stream.hooks",
		Webhook: &domain.WebhookConfig{Port: "8081", Path: "/hook"},
		Restart: &domain.RestartConfig{InitialBackoff: "5 seconds"},
	}
	if err := validateStreamConfig(cfg); err == nil {
		t.Fatal("an unparseable restart.initial_backoff passed validation")
	}
}
