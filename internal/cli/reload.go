package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newReloadCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload the worker definitions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result struct {
				Workers int `json:"workers"`
			}

			if err := newClient().do(cmd.Context(), "POST", "/v1/reload", nil, &result); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%d workers loaded\n", result.Workers)

			return nil
		},
	}
}
