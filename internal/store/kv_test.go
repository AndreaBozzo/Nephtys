package store

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"nephtys/internal/domain"
)

func startTestServer(t *testing.T) (*natsserver.Server, nats.JetStreamContext) {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create test server: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return srv, js
}

func TestNewStreamStore(t *testing.T) {
	_, js := startTestServer(t)

	store, err := NewStreamStore(js)
	if err != nil {
		t.Fatalf("expected store creation to succeed, got: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestPut_Get_RoundTrip(t *testing.T) {
	_, js := startTestServer(t)

	store, err := NewStreamStore(js)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	cfg := domain.StreamSourceConfig{
		ID:    "test-stream",
		Kind:  "websocket",
		URL:   "wss://example.com/ws",
		Topic: "test.topic",
	}

	if err := store.Put(cfg); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := store.Get("test-stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.ID != cfg.ID {
		t.Errorf("expected ID %q, got %q", cfg.ID, got.ID)
	}
	if got.Kind != cfg.Kind {
		t.Errorf("expected Kind %q, got %q", cfg.Kind, got.Kind)
	}
	if got.URL != cfg.URL {
		t.Errorf("expected URL %q, got %q", cfg.URL, got.URL)
	}
}

func TestDelete(t *testing.T) {
	_, js := startTestServer(t)

	store, err := NewStreamStore(js)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	cfg := domain.StreamSourceConfig{ID: "to-delete", Kind: "webhook", Topic: "t"}
	if err := store.Put(cfg); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := store.Delete("to-delete"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err = store.Get("to-delete")
	if err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestList_Empty(t *testing.T) {
	_, js := startTestServer(t)

	store, err := NewStreamStore(js)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	configs, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if configs != nil {
		t.Errorf("expected nil for empty store, got %v", configs)
	}
}

func TestList_Multiple(t *testing.T) {
	_, js := startTestServer(t)

	store, err := NewStreamStore(js)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	for _, id := range []string{"a", "b", "c"} {
		cfg := domain.StreamSourceConfig{ID: id, Kind: "webhook", Topic: "t." + id}
		if err := store.Put(cfg); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}

	configs, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(configs) != 3 {
		t.Errorf("expected 3 configs, got %d", len(configs))
	}
}
