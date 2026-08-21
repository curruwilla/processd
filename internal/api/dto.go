package api

import (
	"time"

	"github.com/curruwilla/processd/internal/config"
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
	MaxAttempts  *int              `json:"max_attempts"`
	Restarts     int               `json:"restarts"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Cwd          string            `json:"cwd"`
	User         string            `json:"user,omitempty"`
	Lock         string            `json:"lock,omitempty"`
	ExitCode     *int              `json:"exit_code"`
	Signal       string            `json:"signal,omitempty"`
	LogTruncated bool              `json:"log_truncated"`
	Usage        *usageResponse    `json:"usage,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	QueuedAt     *time.Time        `json:"queued_at,omitempty"`
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	FinishedAt   *time.Time        `json:"finished_at,omitempty"`
	RetryAt      *time.Time        `json:"retry_at,omitempty"`
	// DurationMS is how long the current attempt has been running, and its final
	// duration once it ended. A running execution reports elapsed time rather
	// than null: for a service, uptime is the number that matters, and it never
	// has a finished attempt to report while it is healthy.
	DurationMS *int64 `json:"duration_ms"`
}

// usageResponse is what a running execution consumes right now, sampled from
// /proc. A finished execution carries none: there is nothing left to read.
type usageResponse struct {
	CPUSeconds float64 `json:"cpu_seconds"`
	RSSBytes   int64   `json:"rss_bytes"`
	Threads    int     `json:"threads"`
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

// healthResponse is the body of GET /v1/health. Store is only filled in by a
// deep check, which is the only one that touches the database.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Store   string `json:"store,omitempty"`
}

// statsResponse is the body of GET /v1/stats.
//
// States counts the non-terminal executions only: a dashboard polls this, and
// counting the whole retained history on every poll would make the endpoint
// slower the longer the node has been running.
type statsResponse struct {
	SlotsUsed  int            `json:"slots_used"`
	SlotsMax   int            `json:"slots_max"`
	Workers    int            `json:"workers"`
	Running    int            `json:"running"`
	QueueDepth int            `json:"queue_depth"`
	States     map[string]int `json:"states"`
	Services   serviceStats   `json:"services"`
}

// serviceStats is what the console needs to tell a healthy node from a flapping
// one. A service produces no terminal state while it is well, so the ordinary
// counters stay silent about it however badly it is behaving.
type serviceStats struct {
	// Up is the services currently running.
	Up int `json:"up"`
	// Restarting is the services waiting out a backoff. They still hold their
	// slots, which is why they are not part of QueueDepth.
	Restarting int `json:"restarting"`
	// Starting is the services between the slot and the first byte of output.
	Starting int `json:"starting"`
	// Restarts totals the restarts accumulated by the live services.
	Restarts int `json:"restarts"`
}

// logLine is one streamed output line, the payload of an SSE "line" event.
type logLine struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// logStreamEnd is the payload of the SSE "end" event, sent once the attempt
// being followed can produce no further output.
type logStreamEnd struct {
	Attempt   int    `json:"attempt"`
	Status    string `json:"status"`
	Truncated bool   `json:"truncated"`
}

// attemptCeiling renders the attempt ceiling of an execution. A service that
// restarts forever has none, and reporting that as a number would make the
// sentinel the client's problem: null says it plainly.
func attemptCeiling(maxAttempts int) *int {
	if maxAttempts == config.AttemptsUnlimited {
		return nil
	}

	return &maxAttempts
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
		MaxAttempts:  attemptCeiling(p.MaxAttempts),
		Restarts:     p.Restarts,
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
		RetryAt:      p.RetryAt,
	}

	if p.PID > 0 {
		pid := p.PID
		resp.PID = &pid
	}

	if elapsed := p.Elapsed(time.Now().UTC()); elapsed > 0 {
		ms := elapsed.Milliseconds()
		resp.DurationMS = &ms
	}

	if resp.Args == nil {
		resp.Args = []string{}
	}

	return resp
}
