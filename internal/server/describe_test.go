package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		URL:      "wss://operator:hunter2@gateway.example.com/feed/p4thtoken?apikey=s3cret&channel=sensors&b4reflag",
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
	for _, secret := range []string{"s3cret", "hunter2", "operator:", "p4thtoken", "b4reflag"} {
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

// Describe reads the config under RLock and redacts it after releasing, which
// is only sound because no sub-config is ever mutated in place — UpdatePipeline
// installs a whole new PipelineConfig rather than editing the old one. That is
// an invariant of the rest of the package, not something Describe enforces, so
// it is worth a test that would fail if the invariant were broken. Under -race
// a concurrent in-place edit is a reported data race rather than a flake.
func TestDescribe_IsSafeAgainstConcurrentPipelineUpdates(t *testing.T) {
	manager := NewStreamManager(nil, newRecordingStore())
	defer manager.StopAll()

	cfg := secretConfig("racing")
	if err := manager.Register(newMockSource(cfg.ID), cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			next := &domain.PipelineConfig{
				Enrich: &domain.EnrichConfig{Tags: map[string]string{"generation": strconv.Itoa(i)}},
			}
			if err := manager.UpdatePipeline(cfg.ID, next); err != nil {
				t.Errorf("update pipeline %d: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			detail, ok := manager.Describe(cfg.ID)
			if !ok {
				t.Error("Describe reported the stream absent")
				return
			}
			// Serialising is what the handler does with the value Describe
			// returns, and it is what makes this exercise the invariant:
			// encoding walks every sub-config pointer, so an in-place edit
			// racing this read is a read and a write of the same struct.
			if _, err := json.Marshal(detail); err != nil {
				t.Errorf("marshal detail: %v", err)
				return
			}
			// Whichever generation is read, it has to be a whole one: the
			// connector fields must never come back half-swapped.
			if detail.Config.Kind != cfg.Kind || detail.Config.Topic != cfg.Topic {
				t.Errorf("Describe saw a torn config: %+v", detail.Config)
				return
			}
		}
	}()

	wg.Wait()
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty stays empty", "", ""},
		{"host kept, path withheld", "wss://stream.example.com/ws", "wss://stream.example.com/[REDACTED]"},
		{"hostless url untouched", "wss://stream.example.com", "wss://stream.example.com"},
		{"root path is structure", "https://api.example.com/", "https://api.example.com/"},
		{"port kept", "wss://gateway.example.com:8443/ws", "wss://gateway.example.com:8443/[REDACTED]"},
		{"userinfo dropped", "wss://user:pass@stream.example.com/ws", "wss://stream.example.com/[REDACTED]"},
		{
			// A token in the path is how a whole class of webhook endpoints
			// authenticates, and nothing in the syntax marks it as one.
			"credential in a path segment withheld",
			"https://hooks.example.com/services/T000/B000/xoxb-s3cret",
			"https://hooks.example.com/[REDACTED]/[REDACTED]/[REDACTED]/[REDACTED]",
		},
		{
			"segment count and trailing slash kept",
			"https://api.example.com/v1/forecast/",
			"https://api.example.com/[REDACTED]/[REDACTED]/",
		},
		{
			"query keys kept, values withheld",
			"https://api.example.com/v1/forecast?latitude=44.49&longitude=11.34",
			"https://api.example.com/[REDACTED]/[REDACTED]?latitude=[REDACTED]&longitude=[REDACTED]",
		},
		{
			"query order preserved",
			"https://api.example.com/p?z=1&a=2&m=3",
			"https://api.example.com/[REDACTED]?z=[REDACTED]&a=[REDACTED]&m=[REDACTED]",
		},
		{
			// No "=" means nothing marks this as a label, so it may be a bare
			// token rather than a flag like ?verbose.
			"valueless parameter withheld whole",
			"https://api.example.com/p?s3cret-token",
			"https://api.example.com/[REDACTED]?[REDACTED]",
		},
		{
			"mixed valued and valueless parameters",
			"https://api.example.com/p?lat=1&verbose",
			"https://api.example.com/[REDACTED]?lat=[REDACTED]&[REDACTED]",
		},
		{"fragment dropped", "https://api.example.com/p#part", "https://api.example.com/[REDACTED]"},
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

// The markers must survive URL assembly readably. url.URL.String() escapes a
// path it is left to encode itself, which turns every marker into
// %5BREDACTED%5D — still redacted, and unreadable in the field an operator is
// reading to identify an endpoint.
func TestRedactURL_MarkersAreNotPercentEscaped(t *testing.T) {
	got := redactURL("https://api.example.com/v1/forecast")

	if strings.Contains(got, "%5B") || strings.Contains(got, "%5D") {
		t.Errorf("redactURL escaped its markers: %q", got)
	}
	if !strings.Contains(got, redactedValue) {
		t.Errorf("redactURL(%q) = %q, want it to contain %q", "https://api.example.com/v1/forecast", got, redactedValue)
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
