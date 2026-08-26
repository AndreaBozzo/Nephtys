package server

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

// TestRedactForAPI covers the path from a connector error to GET /v1/streams.
// The API's auth is optional, and endpoint URLs routinely carry a token.
func TestRedactForAPI(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		absent string
	}{
		{
			name:   "query string token",
			in:     `dial ws://gateway.example.com/feed?token=s3cret: no such host`,
			want:   "ws://gateway.example.com/feed",
			absent: "s3cret",
		},
		{
			name:   "userinfo credentials",
			in:     `Get "https://user:hunter2@api.example.com/v1/events": context deadline exceeded`,
			want:   "https://api.example.com/v1/events",
			absent: "hunter2",
		},
		{
			name:   "trailing punctuation is preserved",
			in:     `connect: unexpected status from https://api.example.com/stream?key=abc.`,
			want:   "https://api.example.com/stream.",
			absent: "key=abc",
		},
		{
			name: "no url is left alone",
			in:   "dial tcp 127.0.0.1:8081: connect: connection refused",
			want: "dial tcp 127.0.0.1:8081: connect: connection refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactForAPI(tt.in)
			if !strings.Contains(got, tt.want) {
				t.Errorf("redactForAPI(%q) = %q, want it to contain %q", tt.in, got, tt.want)
			}
			if tt.absent != "" && strings.Contains(got, tt.absent) {
				t.Errorf("redactForAPI(%q) = %q, which still leaks %q", tt.in, got, tt.absent)
			}
		})
	}
}

// TestLastErrorIsRedacted checks the whole path rather than the helper alone:
// a session failure carrying a credential must not be readable from the API.
func TestLastErrorIsRedacted(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	secret := errors.New(`dial wss://feed.example.com/live?apikey=TOPSECRET: no such host`)
	src := newScriptedSource("leaky", nil, []error{secret})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(0)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForStatus(t, m, src.id, domain.StatusError)

	info := infoFor(t, m, src.id)
	if strings.Contains(info.LastError, "TOPSECRET") {
		t.Errorf("last_error = %q leaks the endpoint credential", info.LastError)
	}
	if !strings.Contains(info.LastError, "feed.example.com") {
		t.Errorf("last_error = %q no longer says which endpoint failed", info.LastError)
	}
}

// TestFailedStreamAlwaysReportsAReason covers a source whose session ends
// without an error of its own. The failure contract promises a reason on every
// failed stream, so it cannot depend on each connector remembering to return
// one.
func TestFailedStreamAlwaysReportsAReason(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	// A nil session error: the session ended, cleanly, on its own.
	src := newScriptedSource("quiet-exit", nil, []error{nil})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(0)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForStatus(t, m, src.id, domain.StatusError)

	info := infoFor(t, m, src.id)
	if info.LastError == "" {
		t.Error("a failed stream reports no reason at all")
	}
	if info.LastErrorAt == nil {
		t.Error("a failed stream reports no last_error_at")
	}
}

// TestCancellationDuringReacquireStopsRatherThanFails covers a stop that lands
// while a restart is re-acquiring. Acquisition fails because the stream is
// being torn down, and that is not a failed attempt: without the check, a
// removal racing a restart reports the stream as errored on its way out.
func TestCancellationDuringReacquireStopsRatherThanFails(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	src := &cancellingSource{id: "cancel-in-open", reacquiring: make(chan struct{})}
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(1)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The first session fails and the supervisor starts its one allowed
	// restart. Wait until it is inside that acquisition.
	select {
	case <-src.reacquiring:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor never re-acquired after the first session failed")
	}

	// Cancel the stream the way Remove and StopAll do, while Open is in flight.
	m.mu.RLock()
	cancel := m.cancels[src.id]
	m.mu.RUnlock()
	if cancel == nil {
		t.Fatal("stream has no cancel installed")
	}
	cancel()

	waitForStatus(t, m, src.id, domain.StatusStopped)

	info := infoFor(t, m, src.id)
	if info.Status == domain.StatusError {
		t.Error("a cancelled acquisition marked the stream errored")
	}
	// One restart was legitimately spent on the first session's failure; the
	// cancelled acquisition must not have spent another.
	if info.RestartCount != 1 {
		t.Errorf("restart_count = %d, want 1 — cancellation is not an attempt", info.RestartCount)
	}
	if got := testutil.ToFloat64(telemetry.StreamRestarts.WithLabelValues(src.id)); got != 1 {
		t.Errorf("restarts counter = %v, want 1", got)
	}
}

// cancellingSource fails its first session and then blocks inside the
// re-acquisition that follows, until the stream is cancelled underneath it.
type cancellingSource struct {
	id          string
	opens       atomic.Int32
	reacquiring chan struct{}
}

func (s *cancellingSource) ID() string { return s.id }

func (s *cancellingSource) Open(ctx context.Context) error {
	if s.opens.Add(1) == 1 {
		return nil
	}
	close(s.reacquiring)
	<-ctx.Done()
	return errors.New("listener gone: stream is shutting down")
}

func (s *cancellingSource) Close() {}

func (s *cancellingSource) Run(ctx context.Context, _ connector.PublishFunc, ready connector.ReadyFunc) error {
	ready()
	return errors.New("first session failed")
}

// TestStopAllClearsEveryStreamSeries checks the cleanup invariant that
// unregistered streams leave no metrics behind. StopAll used to drop only the
// state gauge, so counters outlived the streams they described.
func TestStopAllClearsEveryStreamSeries(t *testing.T) {
	m := NewStreamManager(nil, nil)
	src := newMockSource("stop-all-series")
	registerMock(t, m, src)
	waitForStatus(t, m, src.id, domain.StatusRunning)

	telemetry.StreamRestarts.WithLabelValues(src.id).Inc()
	telemetry.EventsIngested.WithLabelValues(src.id).Inc()

	m.StopAll()

	if telemetry.StreamRestarts.DeleteLabelValues(src.id) {
		t.Error("restart counter survived StopAll")
	}
	if telemetry.EventsIngested.DeleteLabelValues(src.id) {
		t.Error("ingest counter survived StopAll")
	}
	assertStreamStateDeleted(t, src.id)
}

// TestPortClaimIsCanonical covers two spellings of one port. The validator
// accepts both, and both bind the same socket, so keying claims on the raw
// string would let the second stream past the registry and into a bind failure
// that cannot name the holder.
func TestPortClaimIsCanonical(t *testing.T) {
	st := newMemStore()
	m := testManager(t, st, frozenClock())

	port := freePort(t)
	first := webhookCfg("canonical-first", port)
	if err := m.Register(connector.NewWebhookSource(first.ID, first.Topic, first.Webhook), first); err != nil {
		t.Fatalf("register first: %v", err)
	}

	padded := webhookCfg("canonical-second", "0"+port)
	err := m.Register(connector.NewWebhookSource(padded.ID, padded.Topic, padded.Webhook), padded)
	if err == nil {
		t.Fatal("a zero-padded spelling of a claimed port was admitted")
	}
	if !errors.Is(err, ErrPortConflict) {
		t.Errorf("error %v is not an ErrPortConflict — the registry missed the claim", err)
	}
	if !strings.Contains(err.Error(), first.ID) {
		t.Errorf("error %q does not name the holding stream", err)
	}
	if st.has(padded.ID) {
		t.Error("a config was persisted for a stream that was never admitted")
	}
}
