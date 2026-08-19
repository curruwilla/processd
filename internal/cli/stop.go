package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

func newStopCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop an execution gracefully",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values := url.Values{}

			if grace := mustString(cmd, "grace"); grace != "" {
				values.Set("grace", grace)
			}

			path := query("/v1/processes/"+url.PathEscape(args[0]), values)

			if err := newClient().do(cmd.Context(), "DELETE", path, nil, nil); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s stopping\n", args[0])

			return nil
		},
	}

	cmd.Flags().String("grace", "", "how long to wait before SIGKILL, e.g. 15s")

	return cmd
}
