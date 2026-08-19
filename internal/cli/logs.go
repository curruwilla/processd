package cli

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

func newLogsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show the captured output of an execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values := url.Values{}
			values.Set("stream", mustString(cmd, "stream"))

			if attempt := mustInt(cmd, "attempt"); attempt > 0 {
				values.Set("attempt", strconv.Itoa(attempt))
			}

			if tail := mustInt(cmd, "tail"); tail > 0 {
				values.Set("tail", strconv.Itoa(tail))
			}

			path := query("/v1/processes/"+url.PathEscape(args[0])+"/logs", values)

			var body struct {
				Lines     []string `json:"lines"`
				Truncated bool     `json:"truncated"`
			}

			if err := newClient().do(cmd.Context(), "GET", path, nil, &body); err != nil {
				return err
			}

			for _, line := range body.Lines {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}

			if body.Truncated {
				fmt.Fprintln(cmd.ErrOrStderr(), "output was truncated at the configured size cap")
			}

			return nil
		},
	}

	cmd.Flags().String("stream", "both", "stream to read: stdout, stderr or both")
	cmd.Flags().Int("attempt", 0, "attempt to read, defaults to the last one")
	cmd.Flags().Int("tail", 0, "show only the last N lines")

	return cmd
}
