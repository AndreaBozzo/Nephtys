package server

import (
	"bytes"
	"encoding/json"
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

func TestReadJSON_Valid(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"key": "value"})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	w := httptest.NewRecorder()

	var result map[string]string
	err := readJSON(w, req, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["key"] != "value" {
		t.Errorf("expected 'value', got %q", result["key"])
	}
}

func TestReadJSON_Invalid(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()

	var result map[string]string
	err := readJSON(w, req, &result)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
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
