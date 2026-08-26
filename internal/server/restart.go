package server

import (
	"fmt"
	"math"
	"time"

	"nephtys/internal/domain"
)

// Restart defaults. The ladder reproduces the one the pull connectors used to
// run internally, so moving the loop into the supervisor does not change how a
// websocket or sse stream behaves.
const (
	defaultInitialBackoff = 1 * time.Second
	defaultMaxBackoff     = 30 * time.Second
	defaultBackoffFactor  = 2.0
	defaultResetAfter     = 60 * time.Second

	// unlimitedAttempts is the max_attempts value meaning "never give up".
	unlimitedAttempts = -1
)

// restartPolicy is the resolved, validated form of domain.RestartConfig.
type restartPolicy struct {
	maxAttempts    int // unlimitedAttempts, or a count >= 0
	initialBackoff time.Duration
	maxBackoff     time.Duration
	factor         float64
	resetAfter     time.Duration
}

// String makes the policy readable in the log line that records a stream's
// admission.
func (p restartPolicy) String() string {
	attempts := "unlimited"
	if p.maxAttempts != unlimitedAttempts {
		attempts = fmt.Sprintf("%d", p.maxAttempts)
	}
	return fmt.Sprintf("max_attempts=%s backoff=%s..%s x%.1f reset_after=%s",
		attempts, p.initialBackoff, p.maxBackoff, p.factor, p.resetAfter)
}

// next reports the attempt number after a session ended, and whether the
// stream still has budget for it. A false result means the stream is done
// retrying and becomes terminally failed.
func (p restartPolicy) next(attempt int) (int, bool) {
	next := attempt + 1
	if p.maxAttempts == unlimitedAttempts {
		return next, true
	}
	return next, next <= p.maxAttempts
}

// delay returns how long to wait before the given attempt.
func (p restartPolicy) delay(attempt int) time.Duration {
	if attempt <= 1 {
		return p.initialBackoff
	}
	grown := float64(p.initialBackoff) * math.Pow(p.factor, float64(attempt-1))
	return time.Duration(math.Min(grown, float64(p.maxBackoff)))
}

// pullKinds are the connectors whose sessions end on their own and are expected
// to be re-run. Push connectors bind a local listener instead, and losing one
// has always been terminal — so they keep that default and an operator opts in
// to restarts by writing a policy.
var pullKinds = map[string]bool{
	"websocket":   true,
	"sse":         true,
	"rest_poller": true,
}

// restartPolicyFor resolves the effective policy for a stream: the per-kind
// default, overridden field by field by whatever the config sets.
func restartPolicyFor(cfg domain.StreamSourceConfig) restartPolicy {
	policy := restartPolicy{
		maxAttempts:    0,
		initialBackoff: defaultInitialBackoff,
		maxBackoff:     defaultMaxBackoff,
		factor:         defaultBackoffFactor,
		resetAfter:     defaultResetAfter,
	}
	if pullKinds[cfg.Kind] {
		policy.maxAttempts = unlimitedAttempts
	}

	rc := cfg.Restart
	if rc == nil {
		return policy
	}

	if rc.MaxAttempts != nil {
		policy.maxAttempts = *rc.MaxAttempts
	}
	if d, err := time.ParseDuration(rc.InitialBackoff); err == nil && rc.InitialBackoff != "" {
		policy.initialBackoff = d
	}
	if d, err := time.ParseDuration(rc.MaxBackoff); err == nil && rc.MaxBackoff != "" {
		policy.maxBackoff = d
	}
	if rc.Factor > 0 {
		policy.factor = rc.Factor
	}
	if d, err := time.ParseDuration(rc.ResetAfter); err == nil && rc.ResetAfter != "" {
		policy.resetAfter = d
	}
	if policy.maxBackoff < policy.initialBackoff {
		policy.maxBackoff = policy.initialBackoff
	}
	return policy
}
