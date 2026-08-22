package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

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
				Schedule     *struct {
					Cron       string     `json:"cron"`
					NextRun    *time.Time `json:"next_run"`
					MissedRuns int        `json:"missed_runs"`
				} `json:"schedule"`
			}

			if err := newClient().do(cmd.Context(), "GET", "/v1/workers", nil, &workers); err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			defer w.Flush()

			fmt.Fprintln(w, "NAME\tENABLED\tTYPE\tMAX\tSCHEDULE\tNEXT RUN\tCOMMAND")

			for _, worker := range workers {
				schedule, next := "-", "-"

				if worker.Schedule != nil {
					schedule = worker.Schedule.Cron

					if worker.Schedule.NextRun != nil {
						next = worker.Schedule.NextRun.Local().Format(time.RFC3339)
					}

					// A missed occurrence has to be visible where the schedule
					// is, not only in a log nobody is tailing.
					if worker.Schedule.MissedRuns > 0 {
						next = fmt.Sprintf("%s (%d missed)", next, worker.Schedule.MissedRuns)
					}
				}

				fmt.Fprintf(w, "%s\t%t\t%s\t%d\t%s\t%s\t%s\n",
					worker.Name,
					worker.Enabled,
					worker.Type,
					worker.MaxProcesses,
					schedule,
					next,
					worker.Command,
				)
			}

			return nil
		},
	}
}
