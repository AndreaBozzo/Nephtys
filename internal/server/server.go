// Package server provides the Nephtys REST API for stream management.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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
func New(port string, manager *StreamManager, brk *broker.Broker) *Server {
	s := &Server{
		manager: manager,
		broker:  brk,
		logger:  slog.With("component", "server"),
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
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
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}
