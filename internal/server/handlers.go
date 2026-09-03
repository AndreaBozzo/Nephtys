package server

import (
	"errors"
	"fmt"
	"net/http"

	"nephtys/internal/domain"
	"nephtys/internal/pipeline"
)

// handleHealth responds with broker connectivity status.
//
// Superseded by /livez and /readyz, and kept unchanged: it answers
// 200 whatever the broker is doing, which makes it useless as a readiness
// signal and misleading as a liveness one. Existing probes pointed at it keep
// working; new ones should use /livez for "is the process alive" and /readyz
// for "can it accept streams". No removal is scheduled.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	connected := s.broker.IsConnected()

	status := "ok"
	if !connected {
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": status,
		"broker": boolToStatus(connected),
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

// handleGetStream returns one stream's status and the configuration it is
// actually running, including the pipeline currently installed on it.
func (s *Server) handleGetStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "stream id is required")
		return
	}

	detail, ok := s.manager.Describe(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("source %q: %s", id, ErrStreamNotFound))
		return
	}

	writeJSON(w, http.StatusOK, detail)
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

	// Register returned, so the stream holds its resources and its config is
	// durable. Report the lifecycle state alongside the legacy "started" so a
	// caller can tell an already-connected stream from one still dialling,
	// rather than inferring either from the status code.
	//
	// This is a snapshot, not a promise about the future: the supervisor runs
	// concurrently, so the stream may have moved on — or been deleted by
	// another request — by the time the body is written. Admission installs a
	// stream as connecting, which is what an unknown id falls back to rather
	// than an empty field.
	state := domain.StatusConnecting
	if current, ok := s.manager.StatusOf(cfg.ID); ok {
		state = current
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":     cfg.ID,
		"status": "started",
		"state":  string(state),
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

// registerStatus maps a Register failure to a status. A duplicate id, a port
// another stream holds, and a resource the host will not give us are all
// conflicts: the request is well-formed but disagrees with the current state of
// the machine, and no retry of the same config will change that on its own.
//
// Every source.Open failure maps to 409 rather than splitting "address in use"
// from the rest by errno. The taxonomy that split would buy is already in the
// message, and matching errnos across Linux, macOS and Windows is a portability
// liability for no gain.
//
// A config store that would not accept the new stream is not a conflict — the
// caller sent a valid request that a dependency prevented us from honouring.
func registerStatus(err error) int {
	switch {
	case errors.Is(err, ErrStreamExists),
		errors.Is(err, ErrPortConflict),
		errors.Is(err, ErrSourceOpen):
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
