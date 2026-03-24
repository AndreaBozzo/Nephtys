// Package server provides the Nephtys REST API for stream management.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"nephtys/internal/broker"
)

// Server is the Nephtys HTTP server.
type Server struct {
	httpServer *http.Server
	manager    *StreamManager
	broker     *broker.Broker
	logger     *slog.Logger
}

// New creates a new HTTP server wired to the given stream manager and broker.
// If adminToken is non-empty, all endpoints except /health require a valid
// Bearer token in the Authorization header.
func New(port string, manager *StreamManager, brk *broker.Broker, adminToken string) *Server {
	s := &Server{
		manager: manager,
		broker:  brk,
		logger:  slog.With("component", "server"),
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	var handler http.Handler = mux
	handler = bearerAuth(adminToken, map[string]bool{"/health": true, "/metrics": true})(handler)

	s.httpServer = &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s
}

// Start begins listening for HTTP requests. Blocks until the server is shut down.
func (s *Server) Start() error {
	s.logger.Info("REST API listening", "addr", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// registerRoutes wires handlers to the HTTP mux.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/streams", s.handleListStreams)
	mux.HandleFunc("POST /v1/streams", s.handleCreateStream)
	mux.HandleFunc("DELETE /v1/streams/{id}", s.handleDeleteStream)
	mux.HandleFunc("PUT /v1/streams/{id}/pipeline", s.handleUpdatePipeline)
	mux.Handle("GET /metrics", promhttp.Handler())
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("JSON encode error", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	// Prevent DoS: limit incoming request body to 1MB
	r.Body = http.MaxBytesReader(w, r.Body, 1*1024*1024)
	defer func() {
		_ = r.Body.Close()
	}()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}
