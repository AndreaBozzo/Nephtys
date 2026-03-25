package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"nephtys/internal/broker"
	"nephtys/internal/domain"
	"nephtys/internal/store"
)

func startTestNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func connectBroker(t *testing.T, srv *natsserver.Server) *broker.Broker {
	t.Helper()
	brk, err := broker.Connect(srv.ClientURL(), broker.DefaultConfig())
	if err != nil {
		t.Fatalf("broker connect: %v", err)
	}
	t.Cleanup(brk.Close)
	return brk
}

func TestHandleHealth_Connected(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	s := &Server{
		manager: NewStreamManager(brk, nil),
		broker:  brk,
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); body == "" {
		t.Error("expected non-empty health response")
	}
}

func TestRegister_WithBroker(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	manager := NewStreamManager(brk, nil)

	src := &mockSource{id: "reg-test", status: domain.StatusIdle}
	cfg := domain.StreamSourceConfig{ID: "reg-test", Kind: "websocket", Topic: "nephtys.stream.test"}

	if err := manager.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	streams := manager.List()
	if len(streams) != 1 {
		t.Errorf("expected 1 stream, got %d", len(streams))
	}

	manager.StopAll()
}

func TestRegister_WithStore(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	st, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	manager := NewStreamManager(brk, st)

	src := &mockSource{id: "stored-test", status: domain.StatusIdle}
	cfg := domain.StreamSourceConfig{ID: "stored-test", Kind: "webhook", Topic: "nephtys.stream.stored"}

	if err := manager.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Verify it was persisted
	got, err := st.Get("stored-test")
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if got.ID != "stored-test" {
		t.Errorf("expected stored-test, got %s", got.ID)
	}

	manager.StopAll()
}

func TestRegister_Duplicate(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	manager := NewStreamManager(brk, nil)

	src1 := &mockSource{id: "dup", status: domain.StatusIdle}
	cfg := domain.StreamSourceConfig{ID: "dup", Kind: "websocket", Topic: "nephtys.stream.dup"}

	if err := manager.Register(src1, cfg); err != nil {
		t.Fatalf("first register: %v", err)
	}

	src2 := &mockSource{id: "dup", status: domain.StatusIdle}
	if err := manager.Register(src2, cfg); err == nil {
		t.Fatal("expected duplicate error")
	}

	manager.StopAll()
}

func TestRestore_WithStore(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	st, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	// Pre-populate store with a webhook config (webhook doesn't need real connections)
	cfg := domain.StreamSourceConfig{
		ID:      "restore-test",
		Kind:    "webhook",
		Topic:   "nephtys.stream.restored",
		Webhook: &domain.WebhookConfig{Port: "19876", Path: "/hook"},
	}
	if err := st.Put(cfg); err != nil {
		t.Fatalf("store put: %v", err)
	}

	manager := NewStreamManager(brk, st)
	if err := manager.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	streams := manager.List()
	if len(streams) != 1 {
		t.Errorf("expected 1 restored stream, got %d", len(streams))
	}

	manager.StopAll()
}

func TestRestore_NilStore(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	if err := manager.Restore(); err != nil {
		t.Fatalf("restore with nil store should not error: %v", err)
	}
}

func TestServerNew_And_RegisterRoutes(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	manager := NewStreamManager(brk, nil)
	s := New("0", manager, brk, "")

	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.httpServer == nil {
		t.Fatal("expected non-nil http server")
	}
}

func TestServerStartAndShutdown(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	manager := NewStreamManager(brk, nil)
	s := New("0", manager, brk, "")

	// Start in goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start()
	}()

	// Give it a moment to start
	time.Sleep(50 * time.Millisecond)

	// Shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Start should return http.ErrServerClosed
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("expected ErrServerClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for start to return")
	}
}

func TestUpdatePipeline_WithBroker(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	manager := NewStreamManager(brk, nil)

	src := &mockSource{id: "pipe-test", status: domain.StatusIdle}
	cfg := domain.StreamSourceConfig{ID: "pipe-test", Kind: "websocket", Topic: "nephtys.stream.pipe"}

	if err := manager.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	pipelineCfg := &domain.PipelineConfig{
		Filter: &domain.FilterConfig{MatchTypes: []string{"trade"}},
	}
	if err := manager.UpdatePipeline("pipe-test", pipelineCfg); err != nil {
		t.Fatalf("update pipeline: %v", err)
	}

	manager.StopAll()
}
