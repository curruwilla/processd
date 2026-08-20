package api

import (
	"time"

	"github.com/curruwilla/processd/internal/core"
)

// createProcessRequest is the body of POST /v1/processes.
type createProcessRequest struct {
	Worker   string            `json:"worker"`
	Type     core.Type         `json:"type"`
	Params   map[string]string `json:"params"`
	Env      map[string]string `json:"env"`
	Lock     string            `json:"lock"`
	Timeout  string            `json:"timeout"`
	Metadata map[string]string `json:"metadata"`

	// Command and Args are only honoured in execution_mode: raw.
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// createProcessResponse is returned by POST /v1/processes.
type createProcessResponse struct {
	ID      string     `json:"id"`
	Status  core.State `json:"status"`
	PID     *int       `json:"pid"`
	Attempt int        `json:"attempt"`
}

// processResponse is the full representation of an execution.
type processResponse struct {
	ID           string            `json:"id"`
	Worker       string            `json:"worker"`
	Type         core.Type         `json:"type"`
	Status       core.State        `json:"status"`
	Reason       core.Reason       `json:"reason,omitempty"`
	PID          *int              `json:"pid"`
	Attempt      int               `json:"attempt"`
	MaxAttempts  int               `json:"max_attempts"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Cwd          string            `json:"cwd"`
	User         string            `json:"user,omitempty"`
	Lock         string            `json:"lock,omitempty"`
	ExitCode     *int              `json:"exit_code"`
	Signal       string            `json:"signal,omitempty"`
	LogTruncated bool              `json:"log_truncated"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	QueuedAt     *time.Time        `json:"queued_at,omitempty"`
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	FinishedAt   *time.Time        `json:"finished_at,omitempty"`
	DurationMS   *int64            `json:"duration_ms"`
}

// listResponse is one cursor-paginated page of executions.
type listResponse struct {
	Items      []processResponse `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// logsResponse is the body of GET /v1/processes/{id}/logs.
type logsResponse struct {
	Attempt   int      `json:"attempt"`
	Stream    string   `json:"stream"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
}

// signalRequest is the body of POST /v1/processes/{id}/signal. There is no
// per-request choice of target: a signal always reaches the whole process
// group, otherwise grandchildren would survive it.
type signalRequest struct {
	Signal string `json:"signal"`
}

// workerResponse describes a loaded worker, including the params a client may
// send. It never exposes the environment, which may hold secrets.
type workerResponse struct {
	Name         string            `json:"name"`
	Enabled      bool              `json:"enabled"`
	Type         core.Type         `json:"type"`
	Command      string            `json:"command"`
	Params       map[string]string `json:"params"`
	MaxProcesses int               `json:"max_processes"`
}

// healthResponse is the body of GET /v1/health.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// statsResponse is the body of GET /v1/stats.
type statsResponse struct {
	SlotsUsed int `json:"slots_used"`
	SlotsMax  int `json:"slots_max"`
	Workers   int `json:"workers"`
}

// newProcessResponse converts a domain execution into its API shape.
func newProcessResponse(p *core.Process) processResponse {
	resp := processResponse{
		ID:           p.ID,
		Worker:       p.Worker,
		Type:         p.Type,
		Status:       p.State,
		Reason:       p.Reason,
		Attempt:      p.Attempt,
		MaxAttempts:  p.MaxAttempts,
		Command:      p.Command,
		Args:         p.Args,
		Cwd:          p.Cwd,
		User:         p.User,
		Lock:         p.Lock,
		ExitCode:     p.ExitCode,
		Signal:       p.Signal,
		LogTruncated: p.LogTruncated,
		Metadata:     p.Metadata,
		CreatedAt:    p.CreatedAt,
		QueuedAt:     p.QueuedAt,
		StartedAt:    p.StartedAt,
		FinishedAt:   p.FinishedAt,
	}

	if p.PID > 0 {
		pid := p.PID
		resp.PID = &pid
	}

	if duration := p.Duration(); duration > 0 {
		ms := duration.Milliseconds()
		resp.DurationMS = &ms
	}

	if resp.Args == nil {
		resp.Args = []string{}
	}

	return resp
}
