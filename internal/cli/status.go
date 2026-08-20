package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon health and slot usage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClient()

			var health struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			}

			if err := c.do(cmd.Context(), "GET", "/v1/health", nil, &health); err != nil {
				return err
			}

			var stats struct {
				SlotsUsed  int `json:"slots_used"`
				SlotsMax   int `json:"slots_max"`
				Workers    int `json:"workers"`
				Running    int `json:"running"`
				QueueDepth int `json:"queue_depth"`
			}

			if err := c.do(cmd.Context(), "GET", "/v1/stats", nil, &stats); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "status   %s\n", health.Status)
			fmt.Fprintf(cmd.OutOrStdout(), "version  %s\n", health.Version)
			fmt.Fprintf(cmd.OutOrStdout(), "slots    %d/%d\n", stats.SlotsUsed, stats.SlotsMax)
			fmt.Fprintf(cmd.OutOrStdout(), "running  %d\n", stats.Running)
			fmt.Fprintf(cmd.OutOrStdout(), "queued   %d\n", stats.QueueDepth)
			fmt.Fprintf(cmd.OutOrStdout(), "workers  %d\n", stats.Workers)

			return nil
		},
	}
}
