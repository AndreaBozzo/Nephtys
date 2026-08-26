package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

func TestCanonicalPort(t *testing.T) {
	tests := []struct{ in, want string }{
		{"8081", "8081"},
		{"08081", "8081"},
		{"0008081", "8081"},
		// Not a number: the validator rejects it long before a claim is made,
		// and it is keyed on what was written rather than silently reshaped.
		{"http", "http"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := canonicalPort(tt.in); got != tt.want {
			t.Errorf("canonicalPort(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactForAPI_UnparseableURL(t *testing.T) {
	// A URL-looking token that url.Parse rejects must be left alone rather
	// than half-rewritten into something that reads as a different endpoint.
	in := "connect failed: http://[::1: invalid address"
	if got := redactForAPI(in); got != in {
		t.Errorf("redactForAPI(%q) = %q, want it unchanged", in, got)
	}
}

func TestStatusOfUnknownStream(t *testing.T) {
	m := NewStreamManager(nil, nil)
	if status, ok := m.StatusOf("never-registered"); ok {
		t.Errorf("StatusOf reported %q for a stream that was never registered", status)
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("sleepCtx reported cancellation for a delay that simply elapsed")
	}

	// A zero delay is not a cancellation, but a cancelled context is one even
	// with nothing to wait for.
	if !sleepCtx(context.Background(), 0) {
		t.Error("sleepCtx reported cancellation for a zero delay")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(cancelled, 0) {
		t.Error("sleepCtx ignored an already-cancelled context")
	}
	if sleepCtx(cancelled, time.Hour) {
		t.Error("sleepCtx waited out a delay on a cancelled context")
	}
}

// TestRegister_UnbuildablePipelineInstallsNothing covers the first rollback in
// admission: the pipeline is built before anything is acquired, so a stream
// that cannot have one leaves no listener and no stored config behind.
func TestRegister_UnbuildablePipelineInstallsNothing(t *testing.T) {
	st := newMemStore()
	m := testManager(t, st, frozenClock())

	port := freePort(t)
	cfg := webhookCfg("bad-pipeline", port)
	// A duration the pipeline builder rejects. The API validates before it
	// gets here; restore and direct callers do not.
	cfg.Pipeline = &domain.PipelineConfig{
		Dedup: &domain.DedupConfig{Enabled: true, TTL: "5 minutes"},
	}

	err := m.Register(connector.NewWebhookSource(cfg.ID, cfg.Topic, cfg.Webhook), cfg)
	if err == nil {
		t.Fatal("a stream with an unbuildable pipeline was admitted")
	}
	if !strings.Contains(err.Error(), "pipeline") {
		t.Errorf("error %q does not name the pipeline as the cause", err)
	}
	if st.has(cfg.ID) {
		t.Error("a config was persisted for a stream that was never admitted")
	}
	if len(m.List()) != 0 {
		t.Errorf("listed %d streams after a failed registration, want 0", len(m.List()))
	}

	// The port must be free: nothing was acquired, so nothing has to be
	// released.
	sound := webhookCfg("sound-pipeline", port)
	if err := m.Register(connector.NewWebhookSource(sound.ID, sound.Topic, sound.Webhook), sound); err != nil {
		t.Fatalf("port was held by a stream that never started: %v", err)
	}
}

// TestSupervisor_RecoversAfterAFailedReacquire covers the middle of the restart
// path: acquisition fails, the budget still allows another attempt, and the
// next one succeeds.
func TestSupervisor_RecoversAfterAFailedReacquire(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	src := newScriptedSource("reopen-then-recover",
		[]error{nil, errors.New("port briefly taken")},
		[]error{errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(4)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForSessions(t, src, 2)
	waitForStatus(t, m, src.id, domain.StatusRunning)

	info := infoFor(t, m, src.id)
	// Two attempts were spent: the failed session and the failed acquisition.
	if info.RestartCount != 2 {
		t.Errorf("restart_count = %d, want 2", info.RestartCount)
	}
	if opens, _, _ := src.counts(); opens != 3 {
		t.Errorf("acquired %d times, want 3", opens)
	}
}

// TestRegister_GrpcPortIsClaimed checks the claim registry is not
// webhook-specific: gRPC binds a listener on the same terms.
func TestRegister_GrpcPortIsClaimed(t *testing.T) {
	m := testManager(t, newMemStore(), frozenClock())

	port := freePort(t)
	first := domain.StreamSourceConfig{
		ID: "grpc-first", Kind: "grpc", Topic: "nephtys.stream.grpc",
		Grpc: &domain.GrpcConfig{Port: port},
	}
	if err := m.Register(connector.NewGrpcSource(first.ID, first.Topic, first.Grpc), first); err != nil {
		t.Fatalf("register first: %v", err)
	}

	second := domain.StreamSourceConfig{
		ID: "grpc-second", Kind: "grpc", Topic: "nephtys.stream.grpc",
		Grpc: &domain.GrpcConfig{Port: port},
	}
	err := m.Register(connector.NewGrpcSource(second.ID, second.Topic, second.Grpc), second)
	if err == nil {
		t.Fatal("a second gRPC stream took a claimed port")
	}
	if !errors.Is(err, ErrPortConflict) {
		t.Errorf("error %v is not an ErrPortConflict", err)
	}
	if !strings.Contains(err.Error(), first.ID) {
		t.Errorf("error %q does not name the holding stream", err)
	}
}
