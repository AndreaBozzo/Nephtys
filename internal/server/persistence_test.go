package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"nephtys/internal/domain"
	"nephtys/internal/store"
)

// failingStore wraps a real store and fails Put on demand, which is how the
// tests reach the "runtime and stored state must not diverge" path without
// breaking the JetStream connection the rest of the manager depends on.
type failingStore struct {
	inner      configStore
	failPut    bool
	failDelete bool
}

var errStorePut = errors.New("simulated kv failure")

func (f *failingStore) Put(cfg domain.StreamSourceConfig) error {
	if f.failPut {
		return errStorePut
	}
	return f.inner.Put(cfg)
}

func (f *failingStore) Delete(id string) error {
	if f.failDelete {
		return errStorePut
	}
	return f.inner.Delete(id)
}

func (f *failingStore) List() ([]domain.StreamSourceConfig, error) { return f.inner.List() }

// storedGeneration reads back the marker webhookConfig writes, so a test can
// state which pipeline a persisted config describes.
func storedGeneration(cfg domain.StreamSourceConfig) string {
	if cfg.Pipeline == nil || cfg.Pipeline.Enrich == nil {
		return ""
	}
	return cfg.Pipeline.Enrich.Tags["generation"]
}

// freePort reserves and releases a port, returning it for a connector to bind.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close() //nolint:errcheck // best-effort close of temporary listener
	return port
}

// webhookConfig builds a stream whose pipeline stamps generation into every
// event it publishes. The tag is how a test tells which pipeline actually ran,
// rather than which one the manager claims to be running.
func webhookConfig(id, topic, port, generation string) domain.StreamSourceConfig {
	return domain.StreamSourceConfig{
		ID:      id,
		Kind:    "webhook",
		Topic:   topic,
		Webhook: &domain.WebhookConfig{Port: port, Path: "/hook"},
		Pipeline: &domain.PipelineConfig{
			Enrich: &domain.EnrichConfig{Tags: map[string]string{"generation": generation}},
		},
	}
}

// postWebhook delivers one event to a running webhook connector, retrying until
// its listener is up — Register starts the source in a goroutine.
func postWebhook(t *testing.T, port string) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%s/hook", port)
	body := []byte(`{"reading":1}`)

	deadline := time.After(3 * time.Second)
	for {
		resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec,noctx // loopback test request
		if err == nil {
			resp.Body.Close() //nolint:errcheck // response body is unused
			if resp.StatusCode < 300 {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		select {
		case <-deadline:
			t.Fatalf("webhook on port %s never accepted an event: %v", port, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// subscribeGenerations returns a channel yielding the "generation" tag of every
// event published to topic. Subscribing before the stream is registered is what
// makes the assertion about the pipeline that ran, not about timing.
func subscribeGenerations(t *testing.T, nc *nats.Conn, topic string) <-chan string {
	t.Helper()
	got := make(chan string, 8)
	sub, err := nc.Subscribe(topic, func(msg *nats.Msg) {
		var event domain.StreamEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return
		}
		generation, _ := payload["generation"].(string)
		got <- generation
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", topic, err)
	}
	t.Cleanup(func() { sub.Unsubscribe() }) //nolint:errcheck // best-effort unsubscribe
	return got
}

func awaitGeneration(t *testing.T, got <-chan string) string {
	t.Helper()
	select {
	case generation := <-got:
		return generation
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a published event")
		return ""
	}
}

// TestUpdatePipeline_SurvivesRestart is the end-to-end statement of the
// durability contract: a pipeline update that answered 200 is the pipeline a
// restarted process runs. Before pipeline updates were persisted, the restored
// stream came back stamping the registration-time generation — a change the
// operator was told had been applied, silently reverted by a restart.
func TestUpdatePipeline_SurvivesRestart(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	st, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	const topic = "nephtys.stream.durable"
	port := freePort(t)
	generations := subscribeGenerations(t, nc, topic)

	cfg := webhookConfig("durable-pipeline", topic, port, "v1")
	before := NewStreamManager(brk, st)
	source, err := sourceFromConfig(cfg)
	if err != nil {
		t.Fatalf("build source: %v", err)
	}
	if err := before.Register(source, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	updated := &domain.PipelineConfig{
		Enrich: &domain.EnrichConfig{Tags: map[string]string{"generation": "v2"}},
	}
	if err := before.UpdatePipeline(cfg.ID, updated); err != nil {
		t.Fatalf("update pipeline: %v", err)
	}

	// The stored config must already describe the update; the restart below
	// only proves it is acted on.
	stored, err := st.Get(cfg.ID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if stored.Pipeline == nil || storedGeneration(stored) != "v2" {
		t.Fatalf("stored pipeline = %+v, want the updated generation v2", stored.Pipeline)
	}
	// Everything the update did not touch has to survive it.
	if stored.Kind != cfg.Kind || stored.Topic != cfg.Topic || stored.Webhook == nil || stored.Webhook.Port != port {
		t.Fatalf("pipeline update clobbered the connector config: %+v", stored)
	}

	before.StopAll()

	after := NewStreamManager(brk, st)
	if err := after.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(after.StopAll)

	postWebhook(t, port)
	if generation := awaitGeneration(t, generations); generation != "v2" {
		t.Errorf("restored stream stamped generation %q, want %q — the update did not survive the restart", generation, "v2")
	}
}

// TestUpdatePipeline_StoreFailureLeavesStreamOnPreviousPipeline covers the other
// half of the contract: when the update cannot be made durable it must not be
// applied at all. A swap that succeeded while its persistence failed would
// leave the running pipeline and the stored one describing different streams,
// with the divergence invisible until the next restart.
func TestUpdatePipeline_StoreFailureLeavesStreamOnPreviousPipeline(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	inner, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	st := &failingStore{inner: inner}

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	const topic = "nephtys.stream.undurable"
	port := freePort(t)
	generations := subscribeGenerations(t, nc, topic)

	cfg := webhookConfig("undurable-pipeline", topic, port, "v1")
	manager := NewStreamManager(brk, st)
	source, err := sourceFromConfig(cfg)
	if err != nil {
		t.Fatalf("build source: %v", err)
	}
	if err := manager.Register(source, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(manager.StopAll)

	st.failPut = true
	updated := &domain.PipelineConfig{
		Enrich: &domain.EnrichConfig{Tags: map[string]string{"generation": "v2"}},
	}
	err = manager.UpdatePipeline(cfg.ID, updated)
	if err == nil {
		t.Error("expected UpdatePipeline to fail when the config store rejects the write")
	}
	if !errors.Is(err, errStorePut) {
		t.Errorf("error = %v, want it to wrap the store failure", err)
	}
	// A store failure is not a missing stream: the handler distinguishes the
	// two, so a caller polling for 404 must not be told the stream is gone.
	if errors.Is(err, ErrStreamNotFound) {
		t.Error("store failure reported as ErrStreamNotFound")
	}

	// The running pipeline must still be the one the store describes.
	stored, err := inner.Get(cfg.ID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if storedGeneration(stored) != "v1" {
		t.Errorf("stored generation = %q, want v1 — the rejected update was persisted anyway", storedGeneration(stored))
	}

	postWebhook(t, port)
	if generation := awaitGeneration(t, generations); generation != "v1" {
		t.Errorf("running pipeline stamped generation %q, want v1 — the swap happened despite the failed persist", generation)
	}
}

// TestRemove_StoreFailureLeavesStreamRunning is the removal direction of the
// same contract. Tearing a stream down while its persisted config survives is
// not "mostly removed": the next restart brings it back, with no record that a
// removal was ever accepted. The teardown is therefore ordered after the delete,
// and a store that refuses leaves the stream intact.
func TestRemove_StoreFailureLeavesStreamRunning(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	inner, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	st := &failingStore{inner: inner}

	cfg := webhookConfig("undeletable", "nephtys.stream.undeletable", freePort(t), "v1")
	manager := NewStreamManager(brk, st)
	source, err := sourceFromConfig(cfg)
	if err != nil {
		t.Fatalf("build source: %v", err)
	}
	if err := manager.Register(source, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(manager.StopAll)

	st.failDelete = true
	err = manager.Remove(cfg.ID)
	if err == nil {
		t.Fatal("expected Remove to fail when the config store rejects the delete")
	}
	if errors.Is(err, ErrStreamNotFound) {
		t.Error("store failure reported as ErrStreamNotFound")
	}

	// Still registered, and still described by the store — the two agree.
	if streams := manager.List(); len(streams) != 1 || streams[0].ID != cfg.ID {
		t.Errorf("manager.List() = %+v, want the stream still registered", streams)
	}
	if _, err := inner.Get(cfg.ID); err != nil {
		t.Errorf("store no longer holds the stream after a failed removal: %v", err)
	}
}

// TestHandlers_StoreFailureIsNot404 pins the status contract the durability
// rules imply. "The stream is gone" and "the store would not accept the change"
// are different answers: a client that retries on 503 and gives up on 404 must
// not be told a live stream disappeared because JetStream hiccuped.
func TestHandlers_StoreFailureIsNot404(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	inner, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	st := &failingStore{inner: inner}

	cfg := webhookConfig("status-contract", "nephtys.stream.status", freePort(t), "v1")
	manager := NewStreamManager(brk, st)
	source, err := sourceFromConfig(cfg)
	if err != nil {
		t.Fatalf("build source: %v", err)
	}
	if err := manager.Register(source, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(manager.StopAll)

	s := &Server{manager: manager, broker: brk}

	call := func(method, target, body string, handler http.HandlerFunc, id string) int {
		t.Helper()
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		handler(w, req)
		return w.Code
	}

	const pipelineBody = `{"enrich":{"tags":{"generation":"v2"}}}`

	// Unknown stream: still 404, whatever the store is doing.
	if code := call(http.MethodPut, "/v1/streams/ghost/pipeline", pipelineBody, s.handleUpdatePipeline, "ghost"); code != http.StatusNotFound {
		t.Errorf("PUT on unknown stream = %d, want 404", code)
	}

	st.failPut = true
	if code := call(http.MethodPut, "/v1/streams/"+cfg.ID+"/pipeline", pipelineBody, s.handleUpdatePipeline, cfg.ID); code != http.StatusServiceUnavailable {
		t.Errorf("PUT with a failing store = %d, want 503", code)
	}

	// A second stream that cannot be persisted must not read as a conflict:
	// nothing is claiming its id.
	other := webhookConfig("status-contract-2", "nephtys.stream.status2", freePort(t), "v1")
	body, err := json.Marshal(other)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if code := call(http.MethodPost, "/v1/streams", string(body), s.handleCreateStream, ""); code != http.StatusServiceUnavailable {
		t.Errorf("POST with a failing store = %d, want 503", code)
	}

	st.failPut = false
	st.failDelete = true
	if code := call(http.MethodDelete, "/v1/streams/"+cfg.ID, "", s.handleDeleteStream, cfg.ID); code != http.StatusServiceUnavailable {
		t.Errorf("DELETE with a failing store = %d, want 503", code)
	}
	if code := call(http.MethodDelete, "/v1/streams/ghost", "", s.handleDeleteStream, "ghost"); code != http.StatusNotFound {
		t.Errorf("DELETE on unknown stream = %d, want 404", code)
	}
}
