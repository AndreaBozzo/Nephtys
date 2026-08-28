package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nephtys/internal/domain"
)

// recordingStore keeps the last config written for each stream, so a test can
// assert on what would be restarted rather than on what an endpoint reported.
type recordingStore struct {
	mu   sync.Mutex
	last map[string]domain.StreamSourceConfig
}

func newRecordingStore() *recordingStore {
	return &recordingStore{last: make(map[string]domain.StreamSourceConfig)}
}

func (r *recordingStore) Put(cfg domain.StreamSourceConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[cfg.ID] = cfg
	return nil
}

func (r *recordingStore) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.last, id)
	return nil
}

func (r *recordingStore) List() ([]domain.StreamSourceConfig, error) { return nil, nil }

func (r *recordingStore) stored(id string) domain.StreamSourceConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[id]
}

// secretConfig is a config carrying a value in every place the API withholds
// one, so a single fixture covers every branch of the redaction rule.
func secretConfig(id string) domain.StreamSourceConfig {
	return domain.StreamSourceConfig{
		ID:       id,
		Kind:     "websocket",
		Topic:    "nephtys.stream.test",
		URL:      "wss://operator:hunter2@gateway.example.com/ws?apikey=s3cret&channel=sensors",
		Metadata: map[string]string{"site": "turin"},
		Websocket: &domain.WebsocketConfig{
			OnConnectSend: domain.StringList{
				"{\"action\":\"auth\",\"key\":\"s3cret\"}",
				"{\"action\":\"subscribe\"}",
			},
		},
		Sse: &domain.SseConfig{
			Headers: map[string]string{"X-Thing": "s3cret", "Accept": "text/event-stream"},
		},
		RestPoller: &domain.RestPollerConfig{
			Interval: "5s",
			Headers:  map[string]string{"Authorization": "Bearer s3cret"},
		},
		Webhook:  &domain.WebhookConfig{Port: "8081", Path: "/hook", AuthToken: "s3cret"},
		Pipeline: &domain.PipelineConfig{Enrich: &domain.EnrichConfig{Tags: map[string]string{"generation": "first"}}},
	}
}

func getStream(t *testing.T, s *Server, id string) (int, StreamDetail) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/streams/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleGetStream(rec, req)

	if rec.Code != http.StatusOK {
		return rec.Code, StreamDetail{}
	}
	var detail StreamDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	return rec.Code, detail
}

// The acceptance criterion: after a hot swap the endpoint reports the pipeline
// the stream is running, not the one it was registered with.
func TestHandleGetStream_ReportsHotSwappedPipeline(t *testing.T) {
	manager := NewStreamManager(nil, newRecordingStore())
	s := &Server{manager: manager}
	defer manager.StopAll()

	cfg := secretConfig("swapped")
	if err := manager.Register(newMockSource(cfg.ID), cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	code, detail := getStream(t, s, cfg.ID)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if got := storedGeneration(detail.Config); got != "first" {
		t.Fatalf("registered pipeline generation = %q, want %q", got, "first")
	}
	if detail.ID != cfg.ID || detail.Health == "" {
		t.Errorf("detail is missing its StreamInfo fields: %+v", detail.StreamInfo)
	}

	next := &domain.PipelineConfig{Enrich: &domain.EnrichConfig{Tags: map[string]string{"generation": "second"}}}
	if err := manager.UpdatePipeline(cfg.ID, next); err != nil {
		t.Fatalf("update pipeline: %v", err)
	}

	_, detail = getStream(t, s, cfg.ID)
	if got := storedGeneration(detail.Config); got != "second" {
		t.Fatalf("after hot swap the endpoint reported generation %q, want %q", got, "second")
	}

	// A pipeline update replaces one block. Everything else about the stream
	// has to survive it, or the endpoint reports a config nothing is running.
	if detail.Config.Kind != cfg.Kind || detail.Config.Topic != cfg.Topic {
		t.Errorf("hot swap lost connector fields: %+v", detail.Config)
	}
}

func TestHandleGetStream_NotFound(t *testing.T) {
	s := newTestServer()

	if code, _ := getStream(t, s, "absent"); code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", code)
	}
}

func TestHandleGetStream_MissingID(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/v1/streams/", nil)
	rec := httptest.NewRecorder()
	s.handleGetStream(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// The endpoint serves a config, so no value an operator wrote as a secret may
// appear anywhere in its response — not in the field it was written to, and not
// in another field that happens to quote it.
func TestHandleGetStream_ResponseCarriesNoSecret(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	s := &Server{manager: manager}
	defer manager.StopAll()

	cfg := secretConfig("secrets")
	if err := manager.Register(newMockSource(cfg.ID), cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/streams/"+cfg.ID, nil)
	req.SetPathValue("id", cfg.ID)
	rec := httptest.NewRecorder()
	s.handleGetStream(rec, req)

	body := rec.Body.String()
	for _, secret := range []string{"s3cret", "hunter2", "operator:"} {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaks %q:\n%s", secret, body)
		}
	}

	// Emptying the response would pass the check above and leave the endpoint
	// pointless, so assert what it is still required to report.
	for _, kept := range []string{"gateway.example.com", "apikey", "channel", "X-Thing", "8081", "/hook", "turin", "generation"} {
		if !strings.Contains(body, kept) {
			t.Errorf("response body dropped %q, which is not a secret:\n%s", kept, body)
		}
	}
}

// Redaction happens on the way out. Reaching the manager's stored config would
// be invisible here and fatal at the next restart, where the stream would come
// back authenticating with the string "[REDACTED]".
func TestDescribe_DoesNotRedactTheRunningConfig(t *testing.T) {
	store := newRecordingStore()
	manager := NewStreamManager(nil, store)
	defer manager.StopAll()

	cfg := secretConfig("intact")
	if err := manager.Register(newMockSource(cfg.ID), cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, ok := manager.Describe(cfg.ID); !ok {
		t.Fatal("Describe reported the stream absent")
	}

	// UpdatePipeline writes the effective config, so what it stores is what a
	// restart would bring back.
	next := &domain.PipelineConfig{Enrich: &domain.EnrichConfig{Tags: map[string]string{"generation": "second"}}}
	if err := manager.UpdatePipeline(cfg.ID, next); err != nil {
		t.Fatalf("update pipeline: %v", err)
	}

	stored := store.stored(cfg.ID)
	if stored.URL != cfg.URL {
		t.Errorf("persisted url = %q, want %q", stored.URL, cfg.URL)
	}
	if stored.Webhook.AuthToken != "s3cret" {
		t.Errorf("persisted auth_token = %q, want the real one", stored.Webhook.AuthToken)
	}
	if stored.Sse.Headers["X-Thing"] != "s3cret" {
		t.Errorf("persisted sse header = %q, want the real one", stored.Sse.Headers["X-Thing"])
	}
	if stored.RestPoller.Headers["Authorization"] != "Bearer s3cret" {
		t.Errorf("persisted poller header = %q, want the real one", stored.RestPoller.Headers["Authorization"])
	}
	if stored.Websocket.OnConnectSend[0] != cfg.Websocket.OnConnectSend[0] {
		t.Errorf("persisted connect frame = %q, want the real one", stored.Websocket.OnConnectSend[0])
	}
}

// The route sits behind the same bearer auth as every other management route.
// Nothing about the handler opts in — it inherits — so this asserts the wiring
// rather than the middleware.
func TestGetStreamRouteRequiresAuth(t *testing.T) {
	manager := NewStreamManager(nil, nil)
	defer manager.StopAll()

	cfg := secretConfig("guarded")
	if err := manager.Register(newMockSource(cfg.ID), cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	handler := New("0", manager, nil, "token").httpServer.Handler

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"right token", "Bearer token", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/streams/"+cfg.ID, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty stays empty", "", ""},
		{"plain url untouched", "wss://stream.example.com/ws", "wss://stream.example.com/ws"},
		{"userinfo dropped", "wss://user:pass@stream.example.com/ws", "wss://stream.example.com/ws"},
		{
			"query keys kept, values withheld",
			"https://api.example.com/v1/forecast?latitude=44.49&longitude=11.34",
			"https://api.example.com/v1/forecast?latitude=[REDACTED]&longitude=[REDACTED]",
		},
		{
			"query order preserved",
			"https://api.example.com/p?z=1&a=2&m=3",
			"https://api.example.com/p?z=[REDACTED]&a=[REDACTED]&m=[REDACTED]",
		},
		{"valueless flag kept", "https://api.example.com/p?verbose", "https://api.example.com/p?verbose"},
		{"fragment dropped", "https://api.example.com/p#part", "https://api.example.com/p"},
		{"unparseable url withheld whole", "https://example.com/%zz", "[REDACTED]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactURL(tt.raw); got != tt.want {
				t.Errorf("redactURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRedactConfig_WithholdsValuesAndKeepsShape(t *testing.T) {
	cfg := secretConfig("shape")
	got := redactConfig(cfg)

	if got.Webhook.AuthToken != redactedValue {
		t.Errorf("auth_token = %q, want %q", got.Webhook.AuthToken, redactedValue)
	}
	if got.Webhook.Port != "8081" || got.Webhook.Path != "/hook" {
		t.Errorf("webhook lost non-secret fields: %+v", got.Webhook)
	}

	// Every header value goes, the innocuous one included: which header carries
	// the credential is a naming convention we cannot detect, so the rule
	// cannot depend on the name.
	for name, value := range got.Sse.Headers {
		if value != redactedValue {
			t.Errorf("sse header %q = %q, want %q", name, value, redactedValue)
		}
	}
	if len(got.Sse.Headers) != len(cfg.Sse.Headers) {
		t.Errorf("sse headers = %d names, want %d", len(got.Sse.Headers), len(cfg.Sse.Headers))
	}
	if _, ok := got.Sse.Headers["Accept"]; !ok {
		t.Error("sse header names were dropped; the names are the diagnostic")
	}
	if got.RestPoller.Headers["Authorization"] != redactedValue {
		t.Errorf("poller header = %q, want %q", got.RestPoller.Headers["Authorization"], redactedValue)
	}

	if len(got.Websocket.OnConnectSend) != 2 {
		t.Fatalf("on_connect_send = %d frames, want 2", len(got.Websocket.OnConnectSend))
	}
	for i, frame := range got.Websocket.OnConnectSend {
		if frame != redactedValue {
			t.Errorf("frame %d = %q, want %q", i, frame, redactedValue)
		}
	}

	// Fields that cannot carry a credential by construction stay readable.
	if got.RestPoller.Interval != "5s" {
		t.Errorf("interval = %q, want 5s", got.RestPoller.Interval)
	}
	if got.Metadata["site"] != "turin" {
		t.Errorf("metadata = %v, want it intact", got.Metadata)
	}
	if storedGeneration(got) != "first" {
		t.Errorf("the pipeline was redacted; reporting it is what the endpoint is for: %+v", got.Pipeline)
	}
}

// redactConfig takes cfg by value, which copies the struct and shares every
// pointer and map inside it. Only a fresh copy per branch leaves the caller's
// config whole.
func TestRedactConfig_LeavesTheInputAlone(t *testing.T) {
	cfg := secretConfig("input")
	before, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_ = redactConfig(cfg)

	after, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("redactConfig mutated its input:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRedactConfig_AbsentBlocksStayAbsent(t *testing.T) {
	got := redactConfig(domain.StreamSourceConfig{ID: "bare", Kind: "websocket", Topic: "t"})

	if got.Webhook != nil || got.Sse != nil || got.RestPoller != nil || got.Websocket != nil {
		t.Errorf("redaction invented connector blocks: %+v", got)
	}
}
