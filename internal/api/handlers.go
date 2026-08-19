package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/version"
)

// maxRequestBody bounds request bodies; nothing this API accepts is large.
const maxRequestBody = 1 << 20

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.log, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: version.Version,
	})
}

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	used, limit := s.scheduler.Slots().Usage()

	writeJSON(w, s.log, http.StatusOK, statsResponse{
		SlotsUsed: used,
		SlotsMax:  limit,
		Workers:   s.scheduler.Registry().Len(),
	})
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	token, _ := tokenFrom(r.Context())

	workers := []workerResponse{}

	for _, worker := range s.scheduler.Registry().All() {
		if !token.AllowsWorker(worker.Name) {
			continue
		}

		params := make(map[string]string, len(worker.Params))
		for name, param := range worker.Params {
			params[name] = describeParam(param)
		}

		workers = append(workers, workerResponse{
			Name:         worker.Name,
			Enabled:      worker.IsEnabled(),
			Type:         worker.Type,
			Command:      worker.Command,
			Params:       params,
			MaxProcesses: worker.MaxProcesses,
		})
	}

	writeJSON(w, s.log, http.StatusOK, workers)
}

func (s *Server) reloadWorkers(w http.ResponseWriter, r *http.Request) {
	if err := s.reload(r.Context()); err != nil {
		writeError(w, s.log, err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, map[string]int{"workers": s.scheduler.Registry().Len()})
}

func (s *Server) createProcess(w http.ResponseWriter, r *http.Request) {
	var req createProcessRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, s.log, err)
		return
	}

	process, err := s.buildProcess(r, req)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	if err := s.scheduler.Submit(r.Context(), process); err != nil {
		writeError(w, s.log, err)
		return
	}

	status := http.StatusCreated
	if process.State == core.StateQueued {
		status = http.StatusAccepted
	}

	response := createProcessResponse{
		ID:      process.ID,
		Status:  process.State,
		Attempt: process.Attempt,
	}

	if process.PID > 0 {
		pid := process.PID
		response.PID = &pid
	}

	writeJSON(w, s.log, status, response)
}

// buildProcess turns a request into the effective execution definition. The
// definition is frozen here: reloading workers.d later never mutates it.
func (s *Server) buildProcess(r *http.Request, req createProcessRequest) (*core.Process, error) {
	if req.Type == "" {
		req.Type = core.TypeTask
	}

	if req.Type != core.TypeTask {
		return nil, fmt.Errorf("type %q: %w", req.Type, core.ErrUnsupportedType)
	}

	process := &core.Process{
		ID:        core.NewProcessID(),
		Type:      req.Type,
		State:     core.StateCreated,
		Metadata:  req.Metadata,
		CreatedAt: time.Now().UTC(),
	}

	if req.Command != "" {
		if err := s.applyRawCommand(process, req); err != nil {
			return nil, err
		}

		return process, nil
	}

	if req.Worker == "" {
		return nil, badRequest("worker_required", "worker must be set")
	}

	if err := s.applyWorker(r, process, req); err != nil {
		return nil, err
	}

	return process, nil
}

// applyRawCommand handles execution_mode: raw. A client-chosen command is
// remote code execution by design, so it is refused unless the operator opted
// in and allowlisted the exact binary.
func (s *Server) applyRawCommand(process *core.Process, req createProcessRequest) error {
	if s.cfg.ExecutionMode != config.ExecutionModeRaw {
		return fmt.Errorf("execution_mode is %q: %w", s.cfg.ExecutionMode, core.ErrRawCommandDenied)
	}

	if !slices.Contains(s.cfg.AllowedCommands, req.Command) {
		return fmt.Errorf("command %q is not allowlisted: %w", req.Command, core.ErrRawCommandDenied)
	}

	process.Command = req.Command
	process.Args = req.Args
	process.Env = req.Env
	process.Cwd = "/"
	process.Lock = req.Lock
	process.MaxAttempts = 1

	return nil
}

func (s *Server) applyWorker(r *http.Request, process *core.Process, req createProcessRequest) error {
	token, _ := tokenFrom(r.Context())
	if !token.AllowsWorker(req.Worker) {
		return &apiError{
			Status:  http.StatusForbidden,
			Code:    "worker_forbidden",
			Message: fmt.Sprintf("token %q may not use worker %q", token.Name, req.Worker),
		}
	}

	worker, err := s.scheduler.Registry().Get(req.Worker)
	if err != nil {
		return err
	}

	if !worker.IsEnabled() {
		return fmt.Errorf("%q: %w", worker.Name, core.ErrWorkerDisabled)
	}

	resolved, err := worker.Resolve(req.Params)
	if err != nil {
		return err
	}

	process.Worker = worker.Name
	process.Command = worker.Command
	process.Args = resolved.Args
	process.Cwd = worker.Cwd
	process.User = worker.User
	process.Group = worker.Group
	process.Lock = resolved.Lock
	process.Timeout = worker.Timeout.Duration()
	process.MaxAttempts = max(worker.Retry.MaxAttempts, 1)
	process.Env = maps.Clone(worker.Env)

	return applyOverrides(process, worker, req)
}

// applyOverrides applies the request fields a worker explicitly allows to be
// overridden, and rejects the others.
func applyOverrides(process *core.Process, worker *config.Worker, req createProcessRequest) error {
	if len(req.Env) > 0 {
		if !worker.Allows(config.OverridableEnv) {
			return badRequest("override_denied", "worker does not allow env overrides")
		}

		if process.Env == nil {
			process.Env = map[string]string{}
		}

		maps.Copy(process.Env, req.Env)
	}

	if req.Timeout != "" {
		if !worker.Allows(config.OverridableTimeout) {
			return badRequest("override_denied", "worker does not allow timeout overrides")
		}

		timeout, err := time.ParseDuration(req.Timeout)
		if err != nil {
			return badRequest("timeout_invalid", fmt.Sprintf("timeout %q is not a duration", req.Timeout))
		}

		process.Timeout = timeout
	}

	if req.Lock != "" {
		if !worker.Allows(config.OverridableLock) {
			return badRequest("override_denied", "worker does not allow lock overrides")
		}

		process.Lock = req.Lock
	}

	return nil
}

func (s *Server) getProcess(w http.ResponseWriter, r *http.Request) {
	process, err := s.store.GetProcess(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, newProcessResponse(process))
}

func (s *Server) listProcesses(w http.ResponseWriter, r *http.Request) {
	filter, err := parseFilter(r)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	page, err := s.store.ListProcesses(r.Context(), filter)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	items := make([]processResponse, 0, len(page.Items))
	for _, process := range page.Items {
		items = append(items, newProcessResponse(process))
	}

	writeJSON(w, s.log, http.StatusOK, listResponse{Items: items, NextCursor: page.NextCursor})
}

func (s *Server) deleteProcess(w http.ResponseWriter, r *http.Request) {
	grace := s.cfg.ShutdownGrace.Duration()

	if raw := r.URL.Query().Get("grace"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, s.log, badRequest("grace_invalid", fmt.Sprintf("grace %q is not a duration", raw)))
			return
		}

		grace = parsed
	}

	if err := s.supervisor.Stop(r.Context(), r.PathValue("id"), grace); err != nil {
		writeError(w, s.log, err)
		return
	}

	writeJSON(w, s.log, http.StatusAccepted, nil)
}

func (s *Server) signalProcess(w http.ResponseWriter, r *http.Request) {
	var req signalRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, s.log, err)
		return
	}

	if _, err := runner.ParseSignal(req.Signal); err != nil {
		writeError(w, s.log, &apiError{
			Status:  http.StatusBadRequest,
			Code:    "signal_not_allowed",
			Message: err.Error(),
			Details: map[string]any{"allowed": runner.SignalNames()},
		})

		return
	}

	if err := s.supervisor.Signal(r.Context(), r.PathValue("id"), req.Signal); err != nil {
		writeError(w, s.log, err)
		return
	}

	writeJSON(w, s.log, http.StatusAccepted, nil)
}

func (s *Server) processLogs(w http.ResponseWriter, _ *http.Request) {
	// TODO(spec §6.8): resolve the attempt, stream the capped log files.
	writeError(w, s.log, &apiError{
		Status:  http.StatusNotImplemented,
		Code:    "not_implemented",
		Message: "log retrieval is not implemented yet",
	})
}

// decodeJSON reads a bounded, strict JSON body: unknown fields are a client
// error rather than a silently ignored typo.
func decodeJSON(r *http.Request, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(out); err != nil {
		return badRequest("invalid_body", fmt.Sprintf("invalid request body: %v", err))
	}

	return nil
}

func describeParam(param config.Param) string {
	switch {
	case len(param.Enum) > 0:
		return "enum"
	case param.Pattern != "":
		return param.Pattern
	default:
		return "string"
	}
}
