package server

import (
	"testing"
	"time"

	"nephtys/internal/domain"
)

// TestRestartPolicyDefaults pins the backward-compatible defaults. A stream
// registered before restart policies existed has to behave exactly as it did:
// the pull connectors retried forever on a 1s→30s ladder, and a push connector
// that lost its listener stayed down.
func TestRestartPolicyDefaults(t *testing.T) {
	tests := []struct {
		kind        string
		maxAttempts int
	}{
		{"websocket", unlimitedAttempts},
		{"sse", unlimitedAttempts},
		{"rest_poller", unlimitedAttempts},
		{"webhook", 0},
		{"grpc", 0},
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

// TestRestartPolicyMaxBackoffFloor keeps a config that sets a max below the
// initial from producing a ladder that shrinks.
func TestRestartPolicyMaxBackoffFloor(t *testing.T) {
	policy := restartPolicyFor(domain.StreamSourceConfig{
		Kind:    "websocket",
		Restart: &domain.RestartConfig{InitialBackoff: "5s", MaxBackoff: "1s"},
	})
	if policy.maxBackoff != 5*time.Second {
		t.Errorf("max_backoff = %s, want it raised to the initial 5s", policy.maxBackoff)
	}
	if got := policy.delay(3); got != 5*time.Second {
		t.Errorf("delay(3) = %s, want 5s", got)
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
