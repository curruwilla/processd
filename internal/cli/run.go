package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <worker>",
		Short: "Create an execution from a worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params, err := parseParams(mustStringSlice(cmd, "param"))
			if err != nil {
				return err
			}

			request := map[string]any{"worker": args[0], "params": params}

			if lock := mustString(cmd, "lock"); lock != "" {
				request["lock"] = lock
			}

			var created struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				PID    *int   `json:"pid"`
			}

			if err := newClient().do(cmd.Context(), "POST", "/v1/processes", request, &created); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", created.ID, created.Status, formatOptionalInt(created.PID))

			return nil
		},
	}

	cmd.Flags().StringSlice("param", nil, "worker parameter as name=value, repeatable")
	cmd.Flags().String("lock", "", "lock key, when the worker allows overriding it")

	return cmd
}

// parseParams turns name=value pairs into the params object of the API.
func parseParams(raw []string) (map[string]string, error) {
	params := map[string]string{}

	for _, entry := range raw {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("%w: param %q must be written as name=value", errUsage, entry)
		}

		params[name] = value
	}

	return params, nil
}
