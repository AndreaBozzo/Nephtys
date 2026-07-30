package server

import (
	"net/http"

	"nephtys/internal/pipeline"
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
	cfg, err := DecodeStreamConfig(limitBody(w, r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// One validator for every entry point: required fields, connector block, and
	// pipeline. `--config-check` calls the same function, so a config CI accepts
	// is one this handler accepts.
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

	pipelineCfg, err := DecodePipelineConfig(limitBody(w, r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The endpoint that changes behavior on a live stream gets the same
	// validation as the one that creates it. It previously had none.
	if err := pipeline.ValidateConfig(&pipelineCfg); err != nil {
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
