package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/curruwilla/processd/internal/core"
)

// restartPoll is how often a restart checks whether the previous execution has
// settled. Stopping is asynchronous, so the replacement cannot be created until
// the daemon has recorded the outcome and released the slot.
const restartPoll = 250 * time.Millisecond

// restartOptions carries what a restart needs beyond the execution id.
type restartOptions struct {
	id     string
	node   string
	grace  string
	params map[string]string
	wait   time.Duration
}

// processSummary is what a restart needs to know about the execution it
// replaces.
type processSummary struct {
	ID       string            `json:"id"`
	Worker   string            `json:"worker"`
	Type     core.Type         `json:"type"`
	Status   core.State        `json:"status"`
	Metadata map[string]string `json:"metadata"`
}

// createdProcess is the response of POST /v1/processes.
type createdProcess struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	PID    *int   `json:"pid"`
}

func newRestartCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart <id>",
		Short: "Stop an execution and start a new one from the same worker",
		Long: "Restart stops the execution and creates a new one from its worker as\n" +
			"workers.d defines it now, so an edited definition takes effect: a running\n" +
			"execution never picks one up. The replacement is a new execution with its\n" +
			"own id, and its arguments are resolved again, so a worker that declares\n" +
			"params needs them passed with --param.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params, err := parseParams(mustStringSlice(cmd, "param"))
			if err != nil {
				return err
			}

			return restartExecution(cmd.Context(), newClient(), cmd.OutOrStdout(), restartOptions{
				id:     args[0],
				node:   mustString(cmd, "node"),
				grace:  mustString(cmd, "grace"),
				params: params,
				wait:   mustDuration(cmd, "wait"),
			})
		},
	}

	cmd.Flags().String("grace", "", "how long to wait before SIGKILL, e.g. 15s")
	cmd.Flags().StringSlice("param", nil, "worker parameter as name=value, repeatable")
	cmd.Flags().Duration("wait", time.Minute, "how long to wait for the execution to stop and for its slot")
	cmd.Flags().String("node", "", "act on this fleet node instead of the one being addressed")

	return cmd
}

// restartExecution stops an execution and creates its replacement.
//
// The worker is checked before anything is stopped: a definition that was
// removed from workers.d, or disabled there, would otherwise leave the node
// with the execution stopped and nothing left to bring it back.
func restartExecution(ctx context.Context, c *client, out io.Writer, opts restartOptions) error {
	current, err := getProcess(ctx, c, opts.node, opts.id)
	if err != nil {
		return err
	}

	if current.Worker == "" {
		return fmt.Errorf("%w: %s ran a raw command, so there is no worker to restart it from", errUsage, opts.id)
	}

	if err := checkWorker(ctx, c, opts.node, current.Worker); err != nil {
		return err
	}

	deadline := time.Now().Add(opts.wait)

	// An execution that already ended has nothing to stop and holds no slot.
	// Bringing back a service that failed for good is a legitimate restart, so
	// it takes the same command, only without the first half of it.
	if !current.Status.IsTerminal() {
		if err := stopForRestart(ctx, c, opts); err != nil {
			return err
		}

		if err := awaitSettled(ctx, c, opts.node, opts.id, deadline); err != nil {
			return err
		}

		fmt.Fprintf(out, "%s stopped\n", opts.id)
	}

	created, err := createReplacement(ctx, c, current, opts, deadline)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%s %s %s\n", created.ID, created.Status, formatOptionalInt(created.PID))

	return nil
}

// getProcess reads the current representation of an execution.
func getProcess(ctx context.Context, c *client, node, id string) (processSummary, error) {
	var current processSummary

	if err := c.do(ctx, "GET", nodePath(node, "/v1/processes/"+url.PathEscape(id)), nil, &current); err != nil {
		return processSummary{}, err
	}

	return current, nil
}

// checkWorker refuses the restart when the daemon could not create the
// replacement afterwards.
func checkWorker(ctx context.Context, c *client, node, name string) error {
	var workers []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}

	if err := c.do(ctx, "GET", nodePath(node, "/v1/workers"), nil, &workers); err != nil {
		return err
	}

	for _, worker := range workers {
		if worker.Name != name {
			continue
		}

		if !worker.Enabled {
			return fmt.Errorf("worker %q is disabled: nothing was stopped, because nothing would start again", name)
		}

		return nil
	}

	return fmt.Errorf("worker %q is not loaded: nothing was stopped, because nothing would start again", name)
}

// stopForRestart stops the execution, tolerating one that ended between the
// read and the stop: the point is only that it is out of the way.
func stopForRestart(ctx context.Context, c *client, opts restartOptions) error {
	values := url.Values{}

	if opts.grace != "" {
		values.Set("grace", opts.grace)
	}

	path := nodePath(opts.node, query("/v1/processes/"+url.PathEscape(opts.id), values))

	if err := c.do(ctx, "DELETE", path, nil, nil); err != nil && !hasCode(err, "not_running") {
		return err
	}

	return nil
}

// awaitSettled waits for the execution to reach a terminal state. Creating the
// replacement earlier would race the stop: a service takes its slot at
// admission or is refused outright, and the old slot is only free once the
// execution it belonged to has settled.
func awaitSettled(ctx context.Context, c *client, node, id string, deadline time.Time) error {
	for {
		current, err := getProcess(ctx, c, node, id)
		if err != nil {
			return err
		}

		if current.Status.IsTerminal() {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"%s is still %s after the wait window: it was not restarted",
				id, current.Status,
			)
		}

		if err := sleep(ctx, restartPoll); err != nil {
			return err
		}
	}
}

// createReplacement creates the new execution from the worker definition in
// effect now.
//
// It keeps trying while the node reports no free slot: the previous execution
// releases its slot just after the store records it as terminal, so a create
// issued the instant it settles can still find the node full.
func createReplacement(
	ctx context.Context,
	c *client,
	current processSummary,
	opts restartOptions,
	deadline time.Time,
) (createdProcess, error) {
	request := map[string]any{"worker": current.Worker, "params": opts.params}

	// The type says what the execution was, so a worker whose type changed in
	// workers.d fails the restart instead of quietly coming back as the other
	// kind, whose retry and queue semantics are the opposite ones.
	if current.Type != "" {
		request["type"] = current.Type
	}

	// Metadata describes where the work came from, and the replacement is the
	// same work: dropping it would lose that on every restart.
	if len(current.Metadata) > 0 {
		request["metadata"] = current.Metadata
	}

	for {
		var created createdProcess

		// The replacement is dispatched the same way the original was reached:
		// naming the node, never letting the hub choose one.
		err := c.do(ctx, "POST", nodePath(opts.node, "/v1/processes"), request, &created)
		if err == nil {
			return created, nil
		}

		if !hasCode(err, "no_capacity") || !time.Now().Before(deadline) {
			return createdProcess{}, err
		}

		if err := sleep(ctx, restartPoll); err != nil {
			return createdProcess{}, err
		}
	}
}

// sleep waits for d, and returns early when the context is cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
