package server

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"nephtys/internal/domain"
)

// topicPattern matches NATS-safe subject characters.
var topicPattern = regexp.MustCompile(`^[a-zA-Z0-9._>*-]+$`)

// handleHealth responds with broker connectivity status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if !s.broker.IsConnected() {
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": status,
		"broker": boolToStatus(s.broker.IsConnected()),
	})
}

// handleListStreams returns all registered streams and their statuses.
func (s *Server) handleListStreams(w http.ResponseWriter, r *http.Request) {
	streams := s.manager.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"streams": streams,
		"count":   len(streams),
	})
}

// handleCreateStream registers and starts a new stream source.
func (s *Server) handleCreateStream(w http.ResponseWriter, r *http.Request) {
	var cfg domain.StreamSourceConfig
	if err := readJSON(w, r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if cfg.ID == "" || cfg.Kind == "" || cfg.Topic == "" {
		writeError(w, http.StatusBadRequest, "id, kind, and topic are required")
		return
	}

	// URL is required for all except webhook and grpc
	if cfg.Kind != "webhook" && cfg.Kind != "grpc" && cfg.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required for kind "+cfg.Kind)
		return
	}

	if err := validateStreamConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	source, err := sourceFromConfig(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.manager.Register(source, cfg); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":     cfg.ID,
		"status": "started",
	})
}

// handleDeleteStream stops and removes a stream by ID.
func (s *Server) handleDeleteStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "stream id is required")
		return
	}

	if err := s.manager.Remove(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":     id,
		"status": "stopped",
	})
}

// handleUpdatePipeline updates an existing stream's pipeline without downtime.
func (s *Server) handleUpdatePipeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "stream id is required")
		return
	}

	var pipelineCfg domain.PipelineConfig
	if err := readJSON(w, r, &pipelineCfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.manager.UpdatePipeline(id, &pipelineCfg); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":     id,
		"status": "pipeline updated",
	})
}

func boolToStatus(b bool) string {
	if b {
		return "connected"
	}
	return "disconnected"
}

// validateStreamConfig performs input validation on a stream configuration.
func validateStreamConfig(cfg domain.StreamSourceConfig) error {
	if !topicPattern.MatchString(cfg.Topic) {
		return fmt.Errorf("invalid topic %q: must match [a-zA-Z0-9._>*-]+", cfg.Topic)
	}

	// URL validation for connectors that require one
	if cfg.Kind != "webhook" && cfg.Kind != "grpc" {
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return fmt.Errorf("invalid url: %w", err)
		}
		switch cfg.Kind {
		case "websocket":
			if u.Scheme != "ws" && u.Scheme != "wss" {
				return fmt.Errorf("websocket url must use ws:// or wss:// scheme")
			}
		case "rest_poller", "sse":
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("%s url must use http:// or https:// scheme", cfg.Kind)
			}
		}
	}

	// Port validation for webhook and gRPC
	if cfg.Webhook != nil && cfg.Webhook.Port != "" {
		if err := validatePort(cfg.Webhook.Port); err != nil {
			return fmt.Errorf("webhook port: %w", err)
		}
	}
	if cfg.Grpc != nil && cfg.Grpc.Port != "" {
		if err := validatePort(cfg.Grpc.Port); err != nil {
			return fmt.Errorf("grpc port: %w", err)
		}
	}

	return nil
}

func validatePort(s string) error {
	port, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("%q is not a valid port number", s)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d out of range (1-65535)", port)
	}
	return nil
}
