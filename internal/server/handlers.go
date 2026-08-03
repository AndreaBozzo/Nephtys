package server

import (
	"errors"
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
		writeError(w, registerStatus(err), err.Error())
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
		if errors.Is(err, ErrStreamNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		// The config store refused to drop the stream, so it was left running:
		// removing it anyway would resurrect it on the next restart.
		writeError(w, http.StatusServiceUnavailable, err.Error())
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
		if errors.Is(err, ErrStreamNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		// The config store refused the write, so the update was not applied.
		// 503 rather than 404 or 500: the request was well-formed and the
		// stream exists — a dependency is unavailable, and retrying is the
		// right response.
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":     id,
		"status": "pipeline updated",
	})
}

// registerStatus maps a Register failure to a status. A duplicate id is a
// conflict; a config store that would not accept the new stream is not — the
// caller sent a valid request that a dependency prevented us from honouring.
func registerStatus(err error) int {
	switch {
	case errors.Is(err, ErrStreamExists):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

func boolToStatus(b bool) string {
	if b {
		return "connected"
	}
	return "disconnected"
}
