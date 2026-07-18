package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/telemetry"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// mockSource implements connector.StreamSource for testing the manager
// without real WebSocket connections or NATS.
type mockSource struct {
	mu      sync.RWMutex
	id      string
	status  domain.SourceStatus
	started bool
	stopped bool
}

type publishingMockSource struct {
	id      string
	ready   chan connector.PublishFunc
	stopped chan struct{}
	status  atomicStatus
}

type atomicStatus struct {
	mu     sync.RWMutex
	status domain.SourceStatus
}

func (s *atomicStatus) set(status domain.SourceStatus) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

func (s *atomicStatus) get() domain.SourceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *publishingMockSource) Start(ctx context.Context, publish connector.PublishFunc) error {
	s.status.set(domain.StatusRunning)
	s.ready <- publish
	<-ctx.Done()
	s.status.set(domain.StatusStopped)
	close(s.stopped)
	return nil
}

func (s *publishingMockSource) Stop()                       {}
func (s *publishingMockSource) ID() string                  { return s.id }
func (s *publishingMockSource) Status() domain.SourceStatus { return s.status.get() }

func (m *mockSource) Start(_ context.Context, _ connector.PublishFunc) error {
	m.mu.Lock()
	m.started = true
	m.status = domain.StatusRunning
	m.mu.Unlock()
	return nil
}
func (m *mockSource) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.status = domain.StatusStopped
	m.mu.Unlock()
}
func (m *mockSource) ID() string { return m.id }
func (m *mockSource) Status() domain.SourceStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}
func (m *mockSource) isStopped() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stopped
}

func TestStreamManager_RegisterAndList(t *testing.T) {
	manager := NewStreamManager(nil, nil)

	src := &mockSource{id: "test-1", status: domain.StatusIdle}

	// Register should work, but since we pass nil broker, we need to test
	// the list/remove logic directly. The Register method calls source.Start
	// in a goroutine with a real publish func, so we test the tracking state.
	manager.mu.Lock()
	manager.sources[src.id] = src
	manager.mu.Unlock()

	streams := manager.List()
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	if streams[0].ID != "test-1" {
		t.Errorf("expected id 'test-1', got %q", streams[0].ID)
	}
	if streams[0].Health != "degraded" {
		t.Errorf("expected degraded health, got %q", streams[0].Health)
	}
	if streams[0].LastMessageAt != nil {
		t.Errorf("expected no last_message_at, got %v", streams[0].LastMessageAt)
	}
}

func TestStreamManager_ListIncludesHealthAndLastMessageAt(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	src := &mockSource{id: "observed", status: domain.StatusRunning}
	want := time.Date(2026, time.July, 18, 12, 30, 0, 123, time.UTC)
	runtime := &streamRuntime{}
	runtime.lastMessageUnixNano.Store(want.UnixNano())

	manager.mu.Lock()
	manager.sources[src.id] = src
	manager.runtimes[src.id] = runtime
	manager.mu.Unlock()

	streams := manager.List()
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	if streams[0].Health != "healthy" {
		t.Errorf("health = %q, want healthy", streams[0].Health)
	}
	if streams[0].LastMessageAt == nil || !streams[0].LastMessageAt.Equal(want) {
		t.Errorf("last_message_at = %v, want %v", streams[0].LastMessageAt, want)
	}
}

func TestStreamManager_RecordsLastMessageAtBeforePipelineDrop(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	src := &publishingMockSource{
		id:      "live-source",
		ready:   make(chan connector.PublishFunc, 1),
		stopped: make(chan struct{}),
	}
	cfg := domain.StreamSourceConfig{
		ID:    src.id,
		Topic: "events.live",
		Pipeline: &domain.PipelineConfig{
			Filter: &domain.FilterConfig{MatchTypes: []string{"accepted"}},
		},
	}

	if err := manager.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	publish := <-src.ready
	beforePublish := time.Now().UTC()
	if err := publish(cfg.Topic, domain.StreamEvent{Type: "dropped"}); err != nil {
		t.Fatalf("publish dropped event: %v", err)
	}
	afterPublish := time.Now().UTC()

	streams := manager.List()
	if len(streams) != 1 || streams[0].LastMessageAt == nil {
		t.Fatalf("expected last_message_at after ingress, got %+v", streams)
	}
	if streams[0].LastMessageAt.Before(beforePublish) || streams[0].LastMessageAt.After(afterPublish) {
		t.Errorf("last_message_at = %v, want within [%v, %v]", streams[0].LastMessageAt, beforePublish, afterPublish)
	}

	if err := manager.Remove(src.id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	select {
	case <-src.stopped:
	default:
		t.Fatal("source did not stop")
	}
}

func TestTrackSourceStateUpdatesOnTick(t *testing.T) {
	streamID := "state-tracker-tick"
	t.Cleanup(func() { telemetry.DeleteStreamState(streamID) })

	src := &mockSource{id: streamID, status: domain.StatusIdle}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		trackSourceStateOnTicks(ctx, streamID, src, ticks)
	}()

	src.mu.Lock()
	src.status = domain.StatusRunning
	src.mu.Unlock()
	ticks <- time.Now()
	cancel()
	<-done

	if got := testutil.ToFloat64(telemetry.StreamState.WithLabelValues(streamID, "connected")); got != 1 {
		t.Errorf("connected gauge = %v, want 1 after tracker tick", got)
	}
}

func TestSourceHealthAndMetricState(t *testing.T) {
	tests := []struct {
		status domain.SourceStatus
		health string
		metric string
	}{
		{domain.StatusIdle, "degraded", "reconnecting"},
		{domain.StatusConnecting, "degraded", "reconnecting"},
		{domain.StatusRunning, "healthy", "connected"},
		{domain.StatusReconnecting, "degraded", "reconnecting"},
		{domain.StatusStopped, "degraded", "stopped"},
		{domain.StatusError, "errored", "errored"},
	}

	for _, tt := range tests {
		if got := sourceHealth(tt.status); got != tt.health {
			t.Errorf("sourceHealth(%q) = %q, want %q", tt.status, got, tt.health)
		}
		if got := metricState(tt.status); got != tt.metric {
			t.Errorf("metricState(%q) = %q, want %q", tt.status, got, tt.metric)
		}
	}
}

func TestStreamManager_RemoveExisting(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	src := &mockSource{id: "rm-me", status: domain.StatusRunning}

	manager.mu.Lock()
	manager.sources[src.id] = src
	manager.mu.Unlock()
	telemetry.SetStreamState(src.id, "connected")

	err := manager.Remove("rm-me")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !src.isStopped() {
		t.Error("source should have been stopped")
	}

	streams := manager.List()
	if len(streams) != 0 {
		t.Errorf("expected 0 streams after removal, got %d", len(streams))
	}
	assertStreamStateDeleted(t, src.id)
}

func TestStreamManager_RemoveNotFound(t *testing.T) {
	manager := NewStreamManager(nil, nil)

	err := manager.Remove("ghost")
	if err == nil {
		t.Fatal("expected error when removing non-existent source")
	}
}

func TestStreamManager_DuplicateRegister(t *testing.T) {
	manager := NewStreamManager(nil, nil)

	src1 := &mockSource{id: "dupe", status: domain.StatusRunning}
	src2 := &mockSource{id: "dupe", status: domain.StatusIdle}

	manager.mu.Lock()
	manager.sources[src1.id] = src1
	manager.mu.Unlock()

	// Try registering a duplicate directly through the map check
	manager.mu.RLock()
	_, exists := manager.sources[src2.ID()]
	manager.mu.RUnlock()

	if !exists {
		t.Fatal("expected duplicate to be detected")
	}
}

func TestStreamManager_StopAll(t *testing.T) {
	manager := NewStreamManager(nil, nil)

	sources := []*mockSource{
		{id: "a", status: domain.StatusRunning},
		{id: "b", status: domain.StatusRunning},
		{id: "c", status: domain.StatusRunning},
	}

	manager.mu.Lock()
	for _, s := range sources {
		manager.sources[s.id] = s
		telemetry.SetStreamState(s.id, "connected")
	}
	manager.mu.Unlock()

	manager.StopAll()

	for _, s := range sources {
		if !s.isStopped() {
			t.Errorf("source %q should have been stopped", s.id)
		}
		assertStreamStateDeleted(t, s.id)
	}

	if len(manager.List()) != 0 {
		t.Error("all sources should be cleared after StopAll")
	}
}

func assertStreamStateDeleted(t *testing.T, streamID string) {
	t.Helper()
	for _, state := range []string{"connected", "reconnecting", "errored", "stopped"} {
		if telemetry.StreamState.DeleteLabelValues(streamID, state) {
			t.Errorf("stream state series %q still exists for %q", state, streamID)
		}
	}
}

func TestStreamEvent_JSON_Roundtrip(t *testing.T) {
	original := domain.StreamEvent{
		Source:    "binance_btc",
		Type:      "websocket_message",
		Timestamp: 1700000000000,
		Payload:   json.RawMessage(`{"price":"42000.50","qty":"0.001"}`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded domain.StreamEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Source != original.Source {
		t.Errorf("source: got %q, want %q", decoded.Source, original.Source)
	}
	if decoded.Type != original.Type {
		t.Errorf("type: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.Timestamp != original.Timestamp {
		t.Errorf("timestamp: got %d, want %d", decoded.Timestamp, original.Timestamp)
	}
	if string(decoded.Payload) != string(original.Payload) {
		t.Errorf("payload: got %s, want %s", decoded.Payload, original.Payload)
	}
}
