package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/store"
	"github.com/curruwilla/processd/internal/version"
)

const (
	// maxRequestBody bounds request bodies; nothing this API accepts is large.
	maxRequestBody = 1 << 20
	// idempotencyHeader carries the client-chosen key that makes a repeated
	// submission return the original execution instead of starting a new one.
	idempotencyHeader = "Idempotency-Key"
)

// health answers the liveness probe. With ?deep=1 it also proves the store is
// usable: an HTTP server that answers while the database is gone is a liveness
// check that lies (docs/SPEC.md §18).
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	response := healthResponse{Status: "ok", Version: version.Version}

	if !isTruthy(r.URL.Query().Get("deep")) {
		writeJSON(w, s.log, http.StatusOK, response)
		return
	}

	if err := s.store.Ping(r.Context()); err != nil {
		s.log.Error("deep health check failed", slog.Any("error", err))

		response.Status = "degraded"
		response.Store = "unavailable"

		writeJSON(w, s.log, http.StatusServiceUnavailable, response)

		return
	}

	response.Store = "ok"

	writeJSON(w, s.log, http.StatusOK, response)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	byType, err := s.store.CountActiveByTypeAndState(r.Context())
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	restarts, err := s.store.CountRestarts(r.Context())
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	tasks := byType[core.TypeTask]
	services := byType[core.TypeService]

	states := map[string]int{}

	for _, counts := range byType {
		for state, count := range counts {
			states[string(state)] += count
		}
	}

	used, limit := s.scheduler.Slots().Usage()

	writeJSON(w, s.log, http.StatusOK, statsResponse{
		SlotsUsed: used,
		SlotsMax:  limit,
		Workers:   s.scheduler.Registry().Len(),
		Running:   s.supervisor.Running(),
		// A restarting service already holds its slot, so it is not waiting for
		// one: counting it as queue depth would report a full queue on a node
		// whose queue is empty.
		QueueDepth: states[string(core.StateQueued)] + tasks[core.StateRetrying],
		States:     states,
		Services: serviceStats{
			Up:         services[core.StateRunning],
			Restarting: services[core.StateRetrying],
			Starting:   services[core.StateStarting],
			Restarts:   restarts,
		},
	})
}

// isTruthy reads the boolean query parameters of the API, which accept the
// usual spellings rather than only "true".
func isTruthy(raw string) bool {
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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
			Schedule:     s.scheduleOf(worker.Name),
		})
	}

	writeJSON(w, s.log, http.StatusOK, workers)
}

// scheduleOf reports a worker's schedule, or nil when it has none.
func (s *Server) scheduleOf(worker string) *scheduleResponse {
	if s.schedules == nil {
		return nil
	}

	status, ok := s.schedules.Status(worker)
	if !ok {
		return nil
	}

	return &scheduleResponse{
		Cron:         status.Cron,
		Timezone:     status.Timezone,
		NextRun:      status.NextRun,
		LastFiredAt:  status.LastFiredAt,
		LastMissedAt: status.LastMissedAt,
		MissedRuns:   status.MissedRuns,
	}
}

func (s *Server) reloadWorkers(w http.ResponseWriter, r *http.Request) {
	if err := s.reload(r.Context()); err != nil {
		writeError(w, s.log, err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, map[string]int{"workers": s.scheduler.Registry().Len()})
}

func (s *Server) createProcess(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	var req createProcessRequest
	if err := decodeJSON(body, &req); err != nil {
		writeError(w, s.log, err)
		return
	}

	// A named node is dispatched rather than executed here. The hub keeps no
	// authoritative copy of what it forwarded: the execution belongs to the node
	// that runs it, which is what keeps this a router and not a scheduler.
	if req.Node != "" {
		s.dispatchToNode(w, r, req.Node, body)
		return
	}

	key := r.Header.Get(idempotencyHeader)

	replayed, err := s.replayIdempotent(r, key, body)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	if replayed != nil {
		w.Header().Set("Idempotent-Replay", "true")
		writeJSON(w, s.log, http.StatusOK, newCreateResponse(replayed))

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

	s.rememberIdempotent(r, key, body, process.ID)
	s.audit(r, "create", process.ID, process.Worker)

	status := http.StatusCreated
	if process.State == core.StateQueued {
		status = http.StatusAccepted
	}

	writeJSON(w, s.log, status, newCreateResponse(process))
}

// buildProcess turns a request into the effective execution definition. The
// definition is frozen here: reloading workers.d later never mutates it.
func (s *Server) buildProcess(r *http.Request, req createProcessRequest) (*core.Process, error) {
	switch req.Type {
	case "", core.TypeTask, core.TypeService:
	default:
		return nil, fmt.Errorf("type %q: %w", req.Type, core.ErrUnsupportedType)
	}

	if req.Command != "" {
		process := &core.Process{
			ID:        core.NewProcessID(),
			Type:      core.TypeTask,
			State:     core.StateCreated,
			Metadata:  req.Metadata,
			CreatedAt: time.Now().UTC(),
		}

		if err := s.applyRawCommand(process, req); err != nil {
			return nil, err
		}

		return process, nil
	}

	if req.Worker == "" {
		return nil, badRequest("worker_required", "worker must be set")
	}

	return s.buildFromWorker(r, req)
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

	// A raw command is already the sharpest edge in the API. Supervising one
	// forever, with no worker definition bounding it, is not an edge worth
	// adding: a service is declared in workers.d or not at all.
	if req.Type == core.TypeService {
		return fmt.Errorf("type %q: %w", req.Type, core.ErrRawCommandDenied)
	}

	process.Command = req.Command
	process.Args = req.Args
	process.Env = req.Env
	process.Cwd = "/"
	process.Lock = req.Lock
	process.MaxAttempts = 1

	return nil
}

// buildFromWorker resolves a request against a loaded worker. Everything that
// depends on the request — the token, the declared type, the allowed overrides
// — is checked here; turning the template into an execution is the worker's own
// job, so that a scheduled firing produces the same definition.
func (s *Server) buildFromWorker(r *http.Request, req createProcessRequest) (*core.Process, error) {
	token, _ := tokenFrom(r.Context())
	if !token.AllowsWorker(req.Worker) {
		return nil, &apiError{
			Status:  http.StatusForbidden,
			Code:    "worker_forbidden",
			Message: fmt.Sprintf("token %q may not use worker %q", token.Name, req.Worker),
		}
	}

	worker, err := s.scheduler.Registry().Get(req.Worker)
	if err != nil {
		return nil, err
	}

	if !worker.IsEnabled() {
		return nil, fmt.Errorf("%q: %w", worker.Name, core.ErrWorkerDisabled)
	}

	// The worker definition decides what kind of execution this is. A request
	// may state the type, but only to agree with it: running a service worker as
	// a task would silently drop its restart policy.
	if req.Type != "" && req.Type != worker.Type {
		return nil, fmt.Errorf(
			"worker %q is a %s, not a %s: %w",
			worker.Name, worker.Type, req.Type, core.ErrUnsupportedType,
		)
	}

	process, err := worker.Instantiate(req.Params)
	if err != nil {
		return nil, err
	}

	process.Metadata = req.Metadata

	if err := applyOverrides(process, worker, req); err != nil {
		return nil, err
	}

	return process, nil
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
		// A service has no deadline to exceed, so its worker cannot declare a
		// timeout and a request cannot smuggle one in either.
		if process.Type == core.TypeService {
			return badRequest("timeout_denied", "a service has no timeout")
		}

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

	writeJSON(w, s.log, http.StatusOK, s.withUsage(newProcessResponse(process)))
}

func (s *Server) listProcesses(w http.ResponseWriter, r *http.Request) {
	// A hub answers the same endpoint about other nodes when asked to, so that a
	// client reads one API whether or not there is a fleet behind it.
	if node := r.URL.Query().Get("node"); node != "" && s.fleet != nil {
		s.listFleetProcesses(w, r, node)
		return
	}

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
		items = append(items, s.withUsage(newProcessResponse(process)))
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

	id := r.PathValue("id")

	if err := s.supervisor.Stop(r.Context(), id, grace); err != nil {
		writeError(w, s.log, err)
		return
	}

	s.audit(r, "stop", id, grace.String())

	writeJSON(w, s.log, http.StatusAccepted, nil)
}

func (s *Server) signalProcess(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	var req signalRequest
	if err := decodeJSON(body, &req); err != nil {
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

	id := r.PathValue("id")

	if err := s.supervisor.Signal(r.Context(), id, req.Signal); err != nil {
		writeError(w, s.log, err)
		return
	}

	s.audit(r, "signal", id, req.Signal)

	writeJSON(w, s.log, http.StatusAccepted, nil)
}

func (s *Server) processLogs(w http.ResponseWriter, r *http.Request) {
	process, err := s.store.GetProcess(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	stream, err := parseStream(r.URL.Query().Get("stream"))
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	attempt, err := parseAttempt(r.URL.Query().Get("attempt"), process.Attempt)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	tail, err := parsePositive(r.URL.Query().Get("tail"), "tail")
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	// Logs are addressed by the execution's creation time, which never moves,
	// so a retry reads back exactly the attempt it asks for.
	lines, err := s.logs.Lines(process.ID, attempt, stream, process.CreatedAt, tail)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, logsResponse{
		Attempt:   attempt,
		Stream:    string(stream),
		Lines:     lines,
		Truncated: process.LogTruncated,
	})
}

// withUsage attaches a live resource sample to an execution that is running
// here. Anything else is left as it is: /proc holds nothing for a process that
// already exited, and a stale sample would be worse than none.
func (s *Server) withUsage(response processResponse) processResponse {
	usage, ok := s.supervisor.Usage(response.ID)
	if !ok {
		return response
	}

	response.Usage = &usageResponse{
		CPUSeconds: usage.CPUSeconds,
		RSSBytes:   usage.RSSBytes,
		Threads:    usage.Threads,
	}

	return response
}

// replayIdempotent returns the execution a repeated Idempotency-Key already
// produced, or nil when the request is new.
func (s *Server) replayIdempotent(r *http.Request, key string, body []byte) (*core.Process, error) {
	if key == "" {
		return nil, nil
	}

	record, err := s.store.FindIdempotency(r.Context(), key)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if record.RequestHash != hashBody(body) {
		return nil, fmt.Errorf("key %q: %w", key, core.ErrIdempotencyReuse)
	}

	return s.store.GetProcess(r.Context(), record.ProcessID)
}

// rememberIdempotent records the key so a client retry never starts the work
// twice. A failure here is logged, not returned: the execution already exists.
func (s *Server) rememberIdempotent(r *http.Request, key string, body []byte, processID string) {
	if key == "" {
		return
	}

	err := s.store.SaveIdempotency(r.Context(), store.Idempotency{
		Key:         key,
		RequestHash: hashBody(body),
		ProcessID:   processID,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		s.log.Error("saving idempotency key", slog.String("key", key), slog.Any("error", err))
	}
}

// audit records who asked for what. A failure to write the trail must not fail
// the request that already happened.
func (s *Server) audit(r *http.Request, action, processID, detail string) {
	token, _ := tokenFrom(r.Context())

	err := s.store.AppendAudit(r.Context(), store.AuditEntry{
		At:        time.Now().UTC(),
		TokenName: token.Name,
		Action:    action,
		ProcessID: processID,
		Detail:    detail,
	})
	if err != nil {
		s.log.Error("appending audit entry", slog.String("action", action), slog.Any("error", err))
	}
}

// readBody reads a bounded request body. The bytes are kept so that the same
// payload can be both decoded and hashed for idempotency.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRequestBody))
	if err != nil {
		return nil, badRequest("invalid_body", "request body is too large or unreadable")
	}

	return body, nil
}

// decodeJSON decodes a strict JSON body: unknown fields are a client error
// rather than a silently ignored typo.
func decodeJSON(body []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(out); err != nil {
		return badRequest("invalid_body", fmt.Sprintf("invalid request body: %v", err))
	}

	return nil
}

// hashBody fingerprints a request so a repeated idempotency key can be checked
// against the payload it was first used with.
func hashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func parseStream(raw string) (logstore.Stream, error) {
	switch logstore.Stream(raw) {
	case "":
		return logstore.StreamBoth, nil
	case logstore.StreamStdout:
		return logstore.StreamStdout, nil
	case logstore.StreamStderr:
		return logstore.StreamStderr, nil
	case logstore.StreamBoth:
		return logstore.StreamBoth, nil
	default:
		return "", badRequest("stream_unknown", fmt.Sprintf("stream %q is unknown", raw))
	}
}

func parseAttempt(raw string, current int) (int, error) {
	if raw == "" {
		return max(current, 1), nil
	}

	attempt, err := strconv.Atoi(raw)
	if err != nil || attempt < 1 {
		return 0, badRequest("attempt_invalid", fmt.Sprintf("attempt %q is not a positive integer", raw))
	}

	if attempt > current {
		return 0, badRequest("attempt_unknown", fmt.Sprintf("attempt %d has not run", attempt))
	}

	return attempt, nil
}

func parsePositive(raw, name string) (int, error) {
	if raw == "" {
		return 0, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, badRequest(name+"_invalid", fmt.Sprintf("%s %q is not a positive integer", name, raw))
	}

	return value, nil
}

// newCreateResponse renders the answer to a submission.
func newCreateResponse(p *core.Process) createProcessResponse {
	response := createProcessResponse{
		ID:      p.ID,
		Status:  p.State,
		Attempt: p.Attempt,
	}

	if p.PID > 0 {
		pid := p.PID
		response.PID = &pid
	}

	return response
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
