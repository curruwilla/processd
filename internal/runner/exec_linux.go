package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// livenessPollInterval is how often Stop checks whether the group is gone
// before escalating to SIGKILL.
const livenessPollInterval = 250 * time.Millisecond

// ExecRunner runs processes with os/exec.
//
// Every process gets its own process group (Setpgid). Without it, terminating
// an execution would only reach the direct child: grandchildren spawned by a
// script would survive, making max_processes lie and leaking processes past
// graceful shutdown. All signalling therefore targets the group, never the PID.
type ExecRunner struct{}

// NewExecRunner returns the default runner.
func NewExecRunner() *ExecRunner { return &ExecRunner{} }

var _ Runner = (*ExecRunner)(nil)

// Start forks and execs the spec. The command is executed directly — never
// through a shell — so quoting and word splitting cannot be abused.
func (r *ExecRunner) Start(ctx context.Context, spec Spec) (*Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", spec.Command, err)
	}

	if !filepath.IsAbs(spec.Command) {
		return nil, fmt.Errorf("command %q must be an absolute path", spec.Command)
	}

	credential, err := resolveCredential(spec)
	if err != nil {
		return nil, err
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer func() {
		_ = devNull.Close()
	}()

	// Running an operator-defined command is the point of this daemon: the
	// command and its arguments are validated upstream (worker allowlist plus
	// declared, pattern-checked params) and executed directly, never via a shell.
	//
	// exec.CommandContext is deliberately not used: it would tie the process
	// lifetime to a context and kill it outright on cancellation, bypassing the
	// graceful SIGTERM-then-SIGKILL sequence this package owns.
	//nolint:gosec,noctx // see comment above
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = buildEnv(spec)
	cmd.Stdin = devNull
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: credential,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", spec.Command, err)
	}

	pid := cmd.Process.Pid

	startTime, err := processStartTime(pid)
	if err != nil {
		// The process is running; not being able to fingerprint it only costs
		// us the restart-safety check, so report it without killing the work.
		startTime = 0
	}

	return &Handle{
		PID:          pid,
		PIDStartTime: startTime,
		pgid:         pid, // Setpgid makes the child the leader of its own group
		cmd:          cmd,
		done:         make(chan struct{}),
	}, nil
}

// Wait reaps the process and reports how it ended. A process killed by a signal
// reports the conventional 128+N exit code alongside the signal name.
func (r *ExecRunner) Wait(ctx context.Context, h *Handle) (Result, error) {
	defer h.markDone()

	if h.cmd == nil {
		return Result{}, fmt.Errorf("pid %d was not started by this daemon and cannot be waited on", h.PID)
	}

	err := h.cmd.Wait()
	if err == nil {
		return Result{ExitCode: 0}, nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return Result{}, fmt.Errorf("waiting for pid %d: %w", h.PID, err)
	}

	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return Result{ExitCode: exitErr.ExitCode()}, nil
	}

	if status.Signaled() {
		sig := status.Signal()
		return Result{ExitCode: 128 + int(sig), Signal: signalName(sig)}, nil
	}

	return Result{ExitCode: status.ExitStatus()}, nil
}

// Signal delivers a signal from the allowlist to the whole process group.
func (r *ExecRunner) Signal(h *Handle, name string) error {
	sig, err := ParseSignal(name)
	if err != nil {
		return err
	}

	return h.signalGroup(sig)
}

// Usage samples the process through /proc, refusing to answer for a PID that
// is no longer the fingerprinted process.
func (r *ExecRunner) Usage(h *Handle) (Usage, error) {
	return ProcessUsage(h.PID, h.PIDStartTime)
}

// Stop asks the process group to terminate and escalates to SIGKILL once grace
// has elapsed.
func (r *ExecRunner) Stop(ctx context.Context, h *Handle, grace time.Duration) error {
	if err := h.signalGroup(syscall.SIGTERM); err != nil {
		return err
	}

	deadline := time.NewTimer(grace)
	defer deadline.Stop()

	// Processes this daemon started are reaped by Wait, which closes done.
	if h.done != nil {
		select {
		case <-h.done:
			return nil
		case <-ctx.Done():
		case <-deadline.C:
		}

		return h.signalGroup(syscall.SIGKILL)
	}

	// Adopted orphans have no waiter here, so liveness has to be polled.
	ticker := time.NewTicker(livenessPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return h.signalGroup(syscall.SIGKILL)
		case <-deadline.C:
			return h.signalGroup(syscall.SIGKILL)
		case <-ticker.C:
			if !h.groupAlive() {
				return nil
			}
		}
	}
}

// signalGroup sends sig to the process group, never to a bare PID.
func (h *Handle) signalGroup(sig syscall.Signal) error {
	if h.pgid <= 0 {
		return fmt.Errorf("process group of pid %d is unknown", h.PID)
	}

	if err := syscall.Kill(-h.pgid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone
		}

		return fmt.Errorf("signalling group %d with %s: %w", h.pgid, signalName(sig), err)
	}

	return nil
}

// groupAlive reports whether any member of the group is still running.
func (h *Handle) groupAlive() bool {
	return syscall.Kill(-h.pgid, 0) == nil
}

// resolveCredential turns the spec's user and group into uid/gid, and refuses
// to run as root unless that was made explicit.
func resolveCredential(spec Spec) (*syscall.Credential, error) {
	if spec.User == "" {
		if os.Geteuid() == 0 && !spec.AllowRoot {
			return nil, errors.New("refusing to run as root: set a user for the worker or enable allow_root_processes")
		}

		return nil, nil
	}

	account, err := user.Lookup(spec.User)
	if err != nil {
		return nil, fmt.Errorf("looking up user %q: %w", spec.User, err)
	}

	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parsing uid of %q: %w", spec.User, err)
	}

	gid, err := resolveGID(spec, account)
	if err != nil {
		return nil, err
	}

	groups, err := supplementaryGroups(account)
	if err != nil {
		return nil, err
	}

	return &syscall.Credential{
		Uid:    uint32(uid),
		Gid:    gid,
		Groups: groups,
	}, nil
}

func resolveGID(spec Spec, account *user.User) (uint32, error) {
	raw := account.Gid

	if spec.Group != "" {
		group, err := user.LookupGroup(spec.Group)
		if err != nil {
			return 0, fmt.Errorf("looking up group %q: %w", spec.Group, err)
		}

		raw = group.Gid
	}

	gid, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing gid %q: %w", raw, err)
	}

	return uint32(gid), nil
}

func supplementaryGroups(account *user.User) ([]uint32, error) {
	ids, err := account.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("listing groups of %q: %w", account.Username, err)
	}

	groups := make([]uint32, 0, len(ids))

	for _, raw := range ids {
		gid, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing supplementary gid %q: %w", raw, err)
		}

		groups = append(groups, uint32(gid))
	}

	return groups, nil
}

// buildEnv assembles the child environment: the explicit values plus the
// daemon variables the worker opted into. Nothing else crosses over.
func buildEnv(spec Spec) []string {
	env := make([]string, 0, len(spec.Env)+len(spec.EnvPassthrough))

	for _, name := range spec.EnvPassthrough {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	for name, value := range spec.Env {
		env = append(env, name+"="+value)
	}

	return env
}
