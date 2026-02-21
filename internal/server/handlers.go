package server

import (
	"net/http"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
)

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
	if err := readJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if cfg.ID == "" || cfg.Kind == "" || cfg.URL == "" || cfg.Topic == "" {
		writeError(w, http.StatusBadRequest, "id, kind, url, and topic are required")
		return
	}

	var source connector.StreamSource
	switch cfg.Kind {
	case "websocket":
		source = connector.NewWebSocketSource(cfg.ID, cfg.URL, cfg.Topic)
	default:
		writeError(w, http.StatusBadRequest, "unsupported kind: "+cfg.Kind+". Supported: websocket")
		return
	}

	if err := s.manager.Register(source); err != nil {
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

func boolToStatus(b bool) string {
	if b {
		return "connected"
	}
	return "disconnected"
}
