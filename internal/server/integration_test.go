package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

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

	src := newMockSource("reg-test")
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

	src := newMockSource("stored-test")
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

	src1 := newMockSource("dup")
	cfg := domain.StreamSourceConfig{ID: "dup", Kind: "websocket", Topic: "nephtys.stream.dup"}

	if err := manager.Register(src1, cfg); err != nil {
		t.Fatalf("first register: %v", err)
	}

	src2 := newMockSource("dup")
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

// TestRestore_RefusedConfigStaysVisible covers the one path that can start a
// stream without passing through the API's validation. The KV bucket holds
// whatever the version that wrote it accepted, so a config stored before the
// configuration contract tightened must be refused on the way back in — not
// started with a silently reinterpreted pipeline — while its valid siblings
// still restore.
//
// Refused does not mean invisible. At startup there is no caller to answer, so
// a config that stays in the store while its stream is absent from the API is
// the least useful outcome available: the operator sees nothing to delete and
// nothing to explain the missing data. Such a stream is registered in a
// terminal failed state instead, carrying the reason.
func TestRestore_RefusedConfigStaysVisible(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	st, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	// Written by a laxer version: "5 minutes" is not a Go duration, and used to
	// fall back to the 1m default instead of being rejected. Building the
	// pipeline also rejects this one, so it alone would not prove the restore
	// path validates.
	stalePipeline := domain.StreamSourceConfig{
		ID:      "stale-pipeline",
		Kind:    "webhook",
		Topic:   "nephtys.stream.stale.pipeline",
		Webhook: &domain.WebhookConfig{Port: "19877", Path: "/hook"},
		Pipeline: &domain.PipelineConfig{
			Dedup: &domain.DedupConfig{Enabled: true, TTL: "5 minutes"},
		},
	}
	// Invalid for a reason no pipeline build can see: the port is out of range,
	// so without validation here the connector starts and fails asynchronously
	// while the stream reports as registered.
	staleConnector := domain.StreamSourceConfig{
		ID:      "stale-connector",
		Kind:    "webhook",
		Topic:   "nephtys.stream.stale.connector",
		Webhook: &domain.WebhookConfig{Port: "99999", Path: "/hook"},
	}
	good := domain.StreamSourceConfig{
		ID:      "sound-config",
		Kind:    "webhook",
		Topic:   "nephtys.stream.sound",
		Webhook: &domain.WebhookConfig{Port: "19878", Path: "/hook"},
	}
	for _, cfg := range []domain.StreamSourceConfig{stalePipeline, staleConnector, good} {
		if err := st.Put(cfg); err != nil {
			t.Fatalf("store put %s: %v", cfg.ID, err)
		}
	}

	manager := NewStreamManager(brk, st)
	if err := manager.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(manager.StopAll)

	infos := make(map[string]StreamInfo)
	for _, s := range manager.List() {
		infos[s.ID] = s
	}
	if len(infos) != 3 {
		t.Fatalf("listed %d streams, want all 3 persisted ones", len(infos))
	}

	for _, id := range []string{stalePipeline.ID, staleConnector.ID} {
		info := infos[id]
		if info.Status != domain.StatusError {
			t.Errorf("%s status = %q, want %q — the invalid config was started anyway", id, info.Status, domain.StatusError)
		}
		if info.Health != "errored" {
			t.Errorf("%s health = %q, want errored", id, info.Health)
		}
		if info.LastError == "" {
			t.Errorf("%s is failed but reports no reason", id)
		}
	}

	waitForStatus(t, manager, good.ID, domain.StatusRunning)
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

	// Allocate a free port for the test server
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close() //nolint:errcheck // best-effort close of temporary listener

	s := New(port, manager, brk, "")

	// Start in goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start()
	}()

	// Poll until the server is accepting connections (or timeout).
	addr := s.httpServer.Addr
	deadline := time.After(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck // best-effort close of probe connection
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for server to accept connections")
		case <-time.After(10 * time.Millisecond):
		}
	}

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

	src := newMockSource("pipe-test")
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

// TestUpdatePipeline_ConcurrentIngestion covers the hot-swap ordering: events
// arriving while a batching pipeline is being replaced must keep flowing. If
// the old generation is cancelled before the new handler is published, the
// retired batch worker rejects them with "context canceled".
func TestUpdatePipeline_ConcurrentIngestion(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	manager := NewStreamManager(brk, nil)
	t.Cleanup(manager.StopAll)

	batching := func() *domain.PipelineConfig {
		return &domain.PipelineConfig{
			Batch: &domain.BatchConfig{Enabled: true, MaxBatchSize: 50, FlushInterval: "50ms"},
		}
	}

	src := newMockSource("swap-under-load")
	cfg := domain.StreamSourceConfig{
		ID:       src.id,
		Kind:     "websocket",
		Topic:    "nephtys.stream.swap",
		Pipeline: batching(),
	}
	if err := manager.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	publish := <-src.publishes

	var (
		wg       sync.WaitGroup
		stop     = make(chan struct{})
		errMu    sync.Mutex
		firstErr error
		accepted atomic.Int64
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := publish(cfg.Topic, domain.StreamEvent{
					Source:  src.id,
					Type:    "tick",
					Payload: json.RawMessage(`{"v":1}`),
				})
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
				accepted.Add(1)
			}
		}()
	}

	for i := 0; i < 50; i++ {
		if err := manager.UpdatePipeline(src.id, batching()); err != nil {
			t.Errorf("update pipeline: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()

	errMu.Lock()
	if firstErr != nil {
		errMu.Unlock()
		t.Fatalf("ingestion failed during pipeline swap after %d events: %v", accepted.Load(), firstErr)
	}
	errMu.Unlock()

	// Retire the final generation so its buffered batch is flushed, and with it
	// every event still in flight. StopAll is what a shutdown does; running it
	// here rather than leaving it to t.Cleanup makes the count below a
	// statement about a completed handover rather than a race with one.
	manager.StopAll()

	// Absence of errors is not delivery. Every event the source handed to the
	// pipeline was reported to it as accepted, so every one of them has to
	// reach the broker — a stranded event fails no publish and increments no
	// counter, which is exactly what made #57 invisible.
	want := accepted.Load()
	if want == 0 {
		t.Fatal("no events were ingested, so the test asserts nothing")
	}
	got := countPublished(t, brk, cfg.Topic)
	if got != want {
		t.Errorf("broker received %d of %d accepted events — %d were stranded by a pipeline swap", got, want, want-got)
	}
}

// countPublished totals the events the broker holds for a topic, unwrapping
// batched envelopes so the count is in source events rather than messages.
func countPublished(t *testing.T, brk *broker.Broker, topic string) int64 {
	t.Helper()

	sub, err := brk.JetStream().PullSubscribe(topic, "", nats.BindStream("NEPHTYS"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	var total int64
	for {
		msgs, err := sub.Fetch(256, nats.MaxWait(500*time.Millisecond))
		if errors.Is(err, nats.ErrTimeout) {
			return total
		}
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		for _, msg := range msgs {
			total += eventsInMessage(t, msg.Data)
			_ = msg.Ack()
		}
	}
}

// eventsInMessage reports how many source events a published message carries: a
// batch envelope holds a JSON array of payloads, anything else holds one event.
func eventsInMessage(t *testing.T, data []byte) int64 {
	t.Helper()

	var envelope domain.StreamEvent
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var batched []json.RawMessage
	if err := json.Unmarshal(envelope.Payload, &batched); err == nil {
		return int64(len(batched))
	}
	return 1
}
