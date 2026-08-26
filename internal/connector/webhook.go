package connector

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"nephtys/internal/domain"
)

// WebhookSource runs an HTTP server to ingest events via Webhooks.
//
// Reliability model: this is an inbound (push) source. Unlike pull connectors
// (websocket, sse, rest_poller) which reconnect on transient failures, the
// webhook source does not "reconnect" — it accepts whatever the upstream
// client sends, and resending dropped events is the client's responsibility.
//
// The listener is bound in Open, so a port conflict is reported to whoever
// registered the stream instead of being discovered by a goroutine after the
// API has already answered. Restarting a lost listener is the manager's
// decision, driven by the stream's restart policy.
type WebhookSource struct {
	id     string
	topic  string
	config *domain.WebhookConfig
	logger *slog.Logger

	listener net.Listener
	server   *http.Server
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
		logger: slog.With("connector", id, "kind", "webhook"),
	}
}

func (w *WebhookSource) ID() string { return w.id }

// Addr reports the address the listener is bound to. It is only meaningful
// between Open and Close, and exists so a test can bind port 0 and discover
// what it got.
func (w *WebhookSource) Addr() net.Addr {
	if w.listener == nil {
		return nil
	}
	return w.listener.Addr()
}

// Open binds the HTTP listener. A port already taken — by another stream or by
// any other process on the host — fails here, synchronously, with the address
// in the error.
func (w *WebhookSource) Open(ctx context.Context) error {
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", ":"+w.config.Port)
	if err != nil {
		return fmt.Errorf("bind webhook listener: %w", err)
	}
	w.listener = lis
	return nil
}

// Run serves incoming POST requests until the server fails or ctx is cancelled.
func (w *WebhookSource) Run(ctx context.Context, publish PublishFunc, ready ReadyFunc) error {
	lis := w.listener
	if lis == nil {
		return errors.New("webhook source: Run called without a successful Open")
	}

	mux := http.NewServeMux()
	mux.HandleFunc(w.config.Path, w.handleWebhook(publish))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	w.server = srv

	w.logger.Info("Serving Webhook endpoint", "port", w.config.Port, "path", w.config.Path)

	errChan := make(chan error, 1)
	go func() {
		// Serve the locals, not the fields: Close may clear the fields as soon
		// as Run returns, and this goroutine outlives neither.
		errChan <- srv.Serve(lis)
	}()

	// The listener is already bound, so the session is live the moment it is
	// being served.
	ready()

	select {
	case err := <-errChan:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		w.logger.Error("Webhook server failed", "error", err)
		return err
	case <-ctx.Done():
		w.logger.Info("Stopping Webhook server")
		w.shutdown(srv)
		// Wait for Serve to return, so no goroutine of this session survives it.
		<-errChan
		return nil
	}
}

// Close shuts the server down and releases the listener. Safe to call more
// than once, and safe on a source whose Run never started.
func (w *WebhookSource) Close() {
	w.shutdown(w.server)
	if w.listener != nil {
		// Shutdown already closed it in the common path; this covers an Open
		// with no Run.
		_ = w.listener.Close()
		w.listener = nil
	}
}

func (w *WebhookSource) shutdown(srv *http.Server) {
	if srv == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		w.logger.Warn("Failed to gracefully shutdown webhook server", "error", err)
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
			if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expected)) != 1 {
				w.logger.Warn("Unauthorized webhook access attempt")
				http.Error(res, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// Prevent DoS: limit incoming request body to 5MB
		req.Body = http.MaxBytesReader(res, req.Body, 5*1024*1024)

		body, err := io.ReadAll(req.Body)
		if err != nil {
			w.logger.Error("Failed to read webhook body", "error", err)
			http.Error(res, "Bad Request or Payload Too Large", http.StatusBadRequest)
			return
		}
		defer func() {
			if err := req.Body.Close(); err != nil {
				w.logger.Warn("Failed to close webhook body", "error", err)
			}
		}()

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
