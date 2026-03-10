package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"nephtys/internal/domain"
)

// WebhookSource runs an HTTP server to ingest events via Webhooks.
type WebhookSource struct {
	id     string
	topic  string
	config *domain.WebhookConfig
	logger *slog.Logger

	server *http.Server

	mu     sync.RWMutex
	status domain.SourceStatus
	cancel context.CancelFunc
}

// NewWebhookSource creates a new Webhook receiver connector.
func NewWebhookSource(id, topic string, config *domain.WebhookConfig) *WebhookSource {
	if config == nil {
		config = &domain.WebhookConfig{
			Port: "8081",
			Path: "/webhook",
		}
	}
	if config.Port == "" {
		config.Port = "8081"
	}
	if config.Path == "" {
		config.Path = "/webhook"
	}
	if !strings.HasPrefix(config.Path, "/") {
		config.Path = "/" + config.Path
	}

	return &WebhookSource{
		id:     id,
		topic:  topic,
		config: config,
		status: domain.StatusIdle,
		logger: slog.With("connector", id, "kind", "webhook"),
	}
}

func (w *WebhookSource) ID() string { return w.id }

func (w *WebhookSource) Status() domain.SourceStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *WebhookSource) setStatus(s domain.SourceStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = s
}

// Start boots the HTTP server and processes incoming post requests.
func (w *WebhookSource) Start(ctx context.Context, publish PublishFunc) error {
	ctx, w.cancel = context.WithCancel(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc(w.config.Path, w.handleWebhook(publish))

	w.server = &http.Server{
		Addr:    ":" + w.config.Port,
		Handler: mux,
	}

	w.setStatus(domain.StatusRunning)
	w.logger.Info("Starting Webhook server", "port", w.config.Port, "path", w.config.Path)

	errChan := make(chan error, 1)
	go func() {
		if err := w.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.logger.Error("Webhook server failed", "error", err)
			w.setStatus(domain.StatusError)
			errChan <- err
		}
	}()

	for {
		select {
		case err := <-errChan:
			return err
		case <-ctx.Done():
			w.setStatus(domain.StatusStopped)
			w.logger.Info("Stopping Webhook server")
			
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := w.server.Shutdown(shutdownCtx); err != nil {
				w.logger.Warn("Failed to gracefully shutdown webhook server", "error", err)
			}
			return ctx.Err()
		}
	}
}

// handleWebhook returns an http.HandlerFunc that processes incoming POST requests.
func (w *WebhookSource) handleWebhook(publish PublishFunc) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Simple Auth Token Validation
		if w.config.AuthToken != "" {
			authHeader := req.Header.Get("Authorization")
			expected := "Bearer " + w.config.AuthToken
			if authHeader != expected {
				w.logger.Warn("Unauthorized webhook access attempt")
				http.Error(res, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		body, err := io.ReadAll(req.Body)
		if err != nil {
			w.logger.Error("Failed to read webhook body", "error", err)
			http.Error(res, "Bad Request", http.StatusBadRequest)
			return
		}
		defer req.Body.Close()

		if len(body) == 0 {
			http.Error(res, "Empty Body", http.StatusBadRequest)
			return
		}

		// Validate json and repack if necessary
		var jsonPayload json.RawMessage
		if err := json.Unmarshal(body, &jsonPayload); err != nil {
			escapedBody, _ := json.Marshal(string(body))
			jsonPayload = json.RawMessage(escapedBody)
		}

		event := domain.StreamEvent{
			Source:    w.id,
			Type:      "webhook_recv",
			Timestamp: time.Now().UnixMilli(),
			Payload:   jsonPayload,
		}

		if err := publish(w.topic, event); err != nil {
			w.logger.Error("Publish failed", "error", err)
			http.Error(res, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		w.logger.Debug("Webhook event published", "topic", w.topic)
		res.WriteHeader(http.StatusAccepted)
	}
}

// Stop shuts down the webhook HTTP server.
func (w *WebhookSource) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
}
