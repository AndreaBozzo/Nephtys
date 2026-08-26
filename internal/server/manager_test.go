package server

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/telemetry"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// mockSource implements connector.StreamSource for testing the manager
// without real WebSocket connections or NATS.
//
// Its session blocks until the context is cancelled, which is what a real
// connector does. A mock whose Run returned immediately would be restarted
// immediately by the supervisor, so the manager tests would spend their time
// racing a retry loop instead of testing what they claim to.
type mockSource struct {
	id string

	opens    atomic.Int32
	closes   atomic.Int32
	sessions atomic.Int32

	// publishes receives the publish func handed to each session, so a test
	// can push events through the real instrumented path.
	publishes chan connector.PublishFunc
	stopped   chan struct{}
	stopOnce  sync.Once
}

func newMockSource(id string) *mockSource {
	return &mockSource{
		id:        id,
		publishes: make(chan connector.PublishFunc, 4),
		stopped:   make(chan struct{}),
	}
}

func (m *mockSource) ID() string { return m.id }

func (m *mockSource) Open(context.Context) error {
	m.opens.Add(1)
	return nil
}

func (m *mockSource) Close() { m.closes.Add(1) }

func (m *mockSource) Run(ctx context.Context, publish connector.PublishFunc, ready connector.ReadyFunc) error {
	m.sessions.Add(1)
	ready()
	select {
	case m.publishes <- publish:
	default:
	}
	<-ctx.Done()
	m.stopOnce.Do(func() { close(m.stopped) })
	return nil
}

func (m *mockSource) isStopped() bool {
	select {
	case <-m.stopped:
		return true
	default:
		return false
	}
}

// mockConfig is the minimal valid stream config for a mock source.
func mockConfig(id string) domain.StreamSourceConfig {
	return domain.StreamSourceConfig{ID: id, Kind: "websocket", Topic: "nephtys.stream.test"}
}

// registerMock registers a mock source through the real Register path, so the
// manager's per-stream state — including its pipeline generation — is populated
// the way production populates it.
func registerMock(t *testing.T, m *StreamManager, src connector.StreamSource) {
	t.Helper()
	if err := m.Register(src, mockConfig(src.ID())); err != nil {
		t.Fatalf("register %s: %v", src.ID(), err)
	}
}

// waitForStatus blocks until the manager reports want for id. Register returns
// once the stream is admitted, and its first session starts just after, so a
// test that asserts on status has to wait for it rather than race it.
func waitForStatus(t *testing.T, m *StreamManager, id string, want domain.SourceStatus) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		got, ok := m.StatusOf(id)
		if ok && got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("stream %s status = %q, want %q", id, got, want)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStreamManager_RegisterAndList(t *testing.T) {
	manager := NewStreamManager(nil, nil)

	src := newMockSource("test-1")

	// A nil broker is fine here: mockSource never publishes, so the terminal
	// handler is never reached. Going through Register rather than poking the
	// manager's maps keeps every stream's state consistently populated — Remove
	// and StopAll rely on that.
	registerMock(t, manager, src)
	// Register admits the stream and its first session starts just after, so
	// the running state arrives asynchronously. The full status-to-health
	// mapping is covered by TestSourceHealthAndMetricState — what matters here
	// is that List reflects the stream's actual state and that a stream which
	// has never emitted reports no last_message_at.
	waitForStatus(t, manager, "test-1", domain.StatusRunning)

	streams := manager.List()
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	if streams[0].ID != "test-1" {
		t.Errorf("expected id 'test-1', got %q", streams[0].ID)
	}
	if streams[0].Health != "healthy" {
		t.Errorf("expected healthy health, got %q", streams[0].Health)
	}
	if streams[0].LastMessageAt != nil {
		t.Errorf("expected no last_message_at, got %v", streams[0].LastMessageAt)
	}
}

func TestStreamManager_ListIncludesHealthAndLastMessageAt(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	src := newMockSource("observed")
	want := time.Date(2026, time.July, 18, 12, 30, 0, 123, time.UTC)

	registerMock(t, manager, src)
	waitForStatus(t, manager, src.id, domain.StatusRunning)

	manager.mu.RLock()
	manager.runtimes[src.id].lastMessageUnixNano.Store(want.UnixNano())
	manager.mu.RUnlock()

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
	src := newMockSource("live-source")
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
	publish := <-src.publishes
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

// TestStreamStateGaugeFollowsLifecycle checks the gauge is written by the
// transition itself rather than by a poller sampling a source: the manager owns
// the state, so the series has to be right the moment it changes.
func TestStreamStateGaugeFollowsLifecycle(t *testing.T) {
	streamID := "state-gauge"
	t.Cleanup(func() { telemetry.DeleteStreamState(streamID) })

	manager := NewStreamManager(nil, nil)
	src := newMockSource(streamID)
	registerMock(t, manager, src)
	waitForStatus(t, manager, streamID, domain.StatusRunning)

	if got := testutil.ToFloat64(telemetry.StreamState.WithLabelValues(streamID, "connected")); got != 1 {
		t.Errorf("connected gauge = %v, want 1 while the stream is running", got)
	}

	manager.StopAll()
	assertStreamStateDeleted(t, streamID)
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
	src := newMockSource("rm-me")

	registerMock(t, manager, src)
	waitForStatus(t, manager, src.id, domain.StatusRunning)
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

	src1 := newMockSource("dupe")
	src2 := newMockSource("dupe")

	registerMock(t, manager, src1)

	if err := manager.Register(src2, mockConfig(src2.ID())); err == nil {
		t.Fatal("expected a duplicate registration to be rejected")
	}
}

func TestStreamManager_StopAll(t *testing.T) {
	manager := NewStreamManager(nil, nil)

	sources := []*mockSource{newMockSource("a"), newMockSource("b"), newMockSource("c")}

	for _, s := range sources {
		registerMock(t, manager, s)
		waitForStatus(t, manager, s.id, domain.StatusRunning)
	}

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
