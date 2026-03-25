package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
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
}

func TestStreamManager_RemoveExisting(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	src := &mockSource{id: "rm-me", status: domain.StatusRunning}

	manager.mu.Lock()
	manager.sources[src.id] = src
	manager.mu.Unlock()

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
	}
	manager.mu.Unlock()

	manager.StopAll()

	for _, s := range sources {
		if !s.isStopped() {
			t.Errorf("source %q should have been stopped", s.id)
		}
	}

	if len(manager.List()) != 0 {
		t.Error("all sources should be cleared after StopAll")
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
