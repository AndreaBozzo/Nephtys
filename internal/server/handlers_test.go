package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"nephtys/internal/domain"
)

// newTestServer creates a Server with a nil broker and nil store manager for handler testing.
func newTestServer() *Server {
	manager := NewStreamManager(nil, nil)
	return &Server{
		manager: manager,
		broker:  nil,
		logger:  nil,
	}
}

func TestHandleListStreams_Empty(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/v1/streams", nil)
	w := httptest.NewRecorder()

	s.handleListStreams(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if count, ok := body["count"].(float64); !ok || count != 0 {
		t.Errorf("expected count 0, got %v", body["count"])
	}
}

func TestHandleCreateStream_MissingFields(t *testing.T) {
	s := newTestServer()

	tests := []struct {
		name string
		body map[string]string
	}{
		{"missing id", map[string]string{"kind": "websocket", "topic": "t", "url": "wss://x"}},
		{"missing kind", map[string]string{"id": "x", "topic": "t", "url": "wss://x"}},
		{"missing topic", map[string]string{"id": "x", "kind": "websocket", "url": "wss://x"}},
		{"missing url for websocket", map[string]string{"id": "x", "kind": "websocket", "topic": "t"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/v1/streams", bytes.NewReader(body))
			w := httptest.NewRecorder()

			s.handleCreateStream(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleCreateStream_InvalidJSON(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/v1/streams", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()

	s.handleCreateStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// TestHandleCreateStream_RejectsWhatConfigCheckRejects pins the two paths
// together at the HTTP layer: a body `--config-check` refuses must not be
// accepted by the endpoint that starts a stream.
func TestHandleCreateStream_RejectsWhatConfigCheckRejects(t *testing.T) {
	s := newTestServer()

	bodies := map[string]string{
		"unsupported kind":    `{"id":"a","kind":"kafka","url":"https://x/y","topic":"t"}`,
		"misspelled field":    `{"id":"a","kind":"sse","url":"https://x/y","topic":"t","websockets":{}}`,
		"trailing content":    `{"id":"a","kind":"sse","url":"https://x/y","topic":"t"} {"extra":1}`,
		"malformed dedup ttl": `{"id":"a","kind":"sse","url":"https://x/y","topic":"t","pipeline":{"dedup":{"enabled":true,"ttl":"5 minutes"}}}`,
		"mismatched block":    `{"id":"a","kind":"sse","url":"https://x/y","topic":"t","websocket":{"on_connect_send":"{}"}}`,
		"bad poller interval": `{"id":"a","kind":"rest_poller","url":"https://x/y","topic":"t","rest_poller":{"interval":"every 5s"}}`,
		"threshold no path":   `{"id":"a","kind":"sse","url":"https://x/y","topic":"t","pipeline":{"threshold":{"enabled":true}}}`,
		"negative cache size": `{"id":"a","kind":"sse","url":"https://x/y","topic":"t","pipeline":{"dedup":{"enabled":true,"cache_size":-1}}}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			// Same body through the CLI's decode-and-validate path.
			cliRejected := false
			if cfg, err := DecodeStreamConfig(bytes.NewReader([]byte(body))); err != nil {
				cliRejected = true
			} else if err := ValidateStreamConfig(cfg); err != nil {
				cliRejected = true
			}
			if !cliRejected {
				t.Fatalf("--config-check would accept this body, so the test asserts nothing")
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/streams", bytes.NewReader([]byte(body)))
			w := httptest.NewRecorder()
			s.handleCreateStream(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdatePipeline_RejectsInvalidPipeline covers the endpoint that
// changes behavior on a live stream. It previously ran no validation at all, so
// a malformed duration was accepted with 200 and silently reinterpreted.
func TestHandleUpdatePipeline_RejectsInvalidPipeline(t *testing.T) {
	s := newTestServer()

	bodies := map[string]string{
		"malformed flush_interval": `{"batch":{"enabled":true,"flush_interval":"1 sec"}}`,
		"malformed ttl":            `{"dedup":{"enabled":true,"ttl":"5 minutes"}}`,
		"misspelled field":         `{"dedup":{"enabled":true,"tll":"30s"}}`,
		"threshold without path":   `{"threshold":{"enabled":true}}`,
		"empty enrich tags":        `{"enrich":{"tags":{}}}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/v1/streams/ghost/pipeline", bytes.NewReader([]byte(body)))
			req.SetPathValue("id", "ghost")
			w := httptest.NewRecorder()

			s.handleUpdatePipeline(w, req)

			// 400 for a bad body, not the 404 an unknown stream would earn:
			// validation has to run before the stream is looked up, or an
			// operator fixing a typo on a live stream never learns about it.
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleDeleteStream_NotFound(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodDelete, "/v1/streams/nonexistent", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	s.handleDeleteStream(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeleteStream_MissingID(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodDelete, "/v1/streams/", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()

	s.handleDeleteStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleUpdatePipeline_NotFound(t *testing.T) {
	s := newTestServer()

	body, _ := json.Marshal(domain.PipelineConfig{})
	req := httptest.NewRequest(http.MethodPut, "/v1/streams/ghost/pipeline", bytes.NewReader(body))
	req.SetPathValue("id", "ghost")
	w := httptest.NewRecorder()

	s.handleUpdatePipeline(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleUpdatePipeline_MissingID(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodPut, "/v1/streams//pipeline", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()

	s.handleUpdatePipeline(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestValidateStreamConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     domain.StreamSourceConfig
		wantErr bool
	}{
		{
			name: "valid websocket",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "websocket", URL: "wss://example.com/ws", Topic: "my.topic",
			},
			wantErr: false,
		},
		{
			name: "valid rest_poller",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "rest_poller", URL: "https://api.example.com/data", Topic: "my.topic",
			},
			wantErr: false,
		},
		{
			name: "valid webhook",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "webhook", Topic: "my.topic",
				Webhook: &domain.WebhookConfig{Port: "8080", Path: "/hook"},
			},
			wantErr: false,
		},
		{
			name: "invalid topic",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "webhook", Topic: "has spaces!",
			},
			wantErr: true,
		},
		{
			name: "websocket with http scheme",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "websocket", URL: "http://example.com", Topic: "t",
			},
			wantErr: true,
		},
		{
			name: "rest_poller with ws scheme",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "rest_poller", URL: "ws://example.com", Topic: "t",
			},
			wantErr: true,
		},
		{
			name: "invalid webhook port",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "webhook", Topic: "t",
				Webhook: &domain.WebhookConfig{Port: "99999"},
			},
			wantErr: true,
		},
		{
			name: "non-numeric webhook port",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "webhook", Topic: "t",
				Webhook: &domain.WebhookConfig{Port: "abc"},
			},
			wantErr: true,
		},
		{
			name: "invalid grpc port",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "grpc", Topic: "t",
				Grpc: &domain.GrpcConfig{Port: "0"},
			},
			wantErr: true,
		},
		{
			name: "topic with NATS wildcards",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "webhook", Topic: "events.>",
			},
			wantErr: true,
		},
		{
			name: "url with empty host",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "websocket", URL: "wss://", Topic: "t",
			},
			wantErr: true,
		},
		{
			name: "websocket with on_connect_send",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "websocket", URL: "wss://example.com/ws", Topic: "t",
				Websocket: &domain.WebsocketConfig{OnConnectSend: domain.StringList{`{"action":"subscribe"}`}},
			},
			wantErr: false,
		},
		{
			name: "websocket with empty on_connect_send entry",
			cfg: domain.StreamSourceConfig{
				ID: "test", Kind: "websocket", URL: "wss://example.com/ws", Topic: "t",
				Websocket: &domain.WebsocketConfig{OnConnectSend: domain.StringList{""}},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStreamConfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateStreamConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateStreamConfig_Exported sanity-checks the exported wrapper used
// by `nephtys --config-check`. It must agree with the internal validator.
func TestValidateStreamConfig_Exported(t *testing.T) {
	good := domain.StreamSourceConfig{
		ID:    "x",
		Kind:  "websocket",
		URL:   "wss://example.com/ws",
		Topic: "nephtys.stream.x",
	}
	if err := ValidateStreamConfig(good); err != nil {
		t.Errorf("ValidateStreamConfig(good) = %v, want nil", err)
	}

	bad := domain.StreamSourceConfig{
		ID:    "x",
		Kind:  "websocket",
		URL:   "http://wrong-scheme",
		Topic: "nephtys.stream.x",
	}
	if err := ValidateStreamConfig(bad); err == nil {
		t.Error("ValidateStreamConfig(bad) = nil, want error")
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "bad input")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["error"] != "bad input" {
		t.Errorf("expected error 'bad input', got %q", body["error"])
	}
}

func TestLimitBody_CapsRequestBody(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), 2*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	w := httptest.NewRecorder()

	read, err := io.ReadAll(limitBody(w, req))
	if err == nil {
		t.Fatalf("expected an error past the 1MB cap, read %d bytes cleanly", len(read))
	}
	if len(read) > 1*1024*1024 {
		t.Errorf("read %d bytes, want no more than the 1MB cap", len(read))
	}
}

func TestBoolToStatus(t *testing.T) {
	if got := boolToStatus(true); got != "connected" {
		t.Errorf("expected connected, got %s", got)
	}
	if got := boolToStatus(false); got != "disconnected" {
		t.Errorf("expected disconnected, got %s", got)
	}
}

func TestSourceFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     domain.StreamSourceConfig
		wantErr bool
	}{
		{
			name: "websocket",
			cfg:  domain.StreamSourceConfig{ID: "ws", Kind: "websocket", URL: "wss://x", Topic: "t"},
		},
		{
			name: "rest_poller",
			cfg:  domain.StreamSourceConfig{ID: "rp", Kind: "rest_poller", URL: "https://x", Topic: "t"},
		},
		{
			name: "webhook",
			cfg:  domain.StreamSourceConfig{ID: "wh", Kind: "webhook", Topic: "t"},
		},
		{
			name: "grpc",
			cfg:  domain.StreamSourceConfig{ID: "gr", Kind: "grpc", Topic: "t"},
		},
		{
			name: "sse",
			cfg:  domain.StreamSourceConfig{ID: "ss", Kind: "sse", URL: "https://x", Topic: "t"},
		},
		{
			name:    "unsupported",
			cfg:     domain.StreamSourceConfig{ID: "u", Kind: "mqtt", Topic: "t"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := sourceFromConfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && src == nil {
				t.Error("expected non-nil source")
			}
		})
	}
}

func TestHandleCreateStream_ValidationErrors(t *testing.T) {
	s := newTestServer()

	// Invalid topic
	cfg := map[string]string{
		"id": "test", "kind": "webhook", "topic": "has spaces!",
	}
	body, _ := json.Marshal(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/streams", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.handleCreateStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdatePipeline_InvalidJSON(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodPut, "/v1/streams/x/pipeline", bytes.NewReader([]byte("bad")))
	req.SetPathValue("id", "x")
	w := httptest.NewRecorder()

	s.handleUpdatePipeline(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
