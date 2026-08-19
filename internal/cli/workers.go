package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newWorkersCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "workers",
		Short: "List the workers the daemon has loaded",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var workers []struct {
				Name         string `json:"name"`
				Enabled      bool   `json:"enabled"`
				Type         string `json:"type"`
				Command      string `json:"command"`
				MaxProcesses int    `json:"max_processes"`
			}

			if err := newClient().do(cmd.Context(), "GET", "/v1/workers", nil, &workers); err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			defer w.Flush()

			fmt.Fprintln(w, "NAME\tENABLED\tTYPE\tMAX\tCOMMAND")

			for _, worker := range workers {
				fmt.Fprintf(w, "%s\t%t\t%s\t%d\t%s\n",
					worker.Name,
					worker.Enabled,
					worker.Type,
					worker.MaxProcesses,
					worker.Command,
				)
			}

			return nil
		},
	}
}
