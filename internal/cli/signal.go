package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

func newSignalCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "signal <id> <signal>",
		Short: "Send a signal to a running execution",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/v1/processes/" + url.PathEscape(args[0]) + "/signal"
			body := map[string]string{"signal": args[1]}

			if err := newClient().do(cmd.Context(), "POST", path, body, nil); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s sent to %s\n", args[1], args[0])

			return nil
		},
	}
}
