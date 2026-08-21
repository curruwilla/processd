package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// processRow is the subset of the process representation the CLI prints.
type processRow struct {
	ID       string `json:"id"`
	Worker   string `json:"worker"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	PID      *int   `json:"pid"`
	Attempt  int    `json:"attempt"`
	ExitCode *int   `json:"exit_code"`
}

func newPsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List executions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			values := url.Values{}

			for _, status := range mustStringSlice(cmd, "status") {
				values.Add("status", status)
			}

			if worker := mustString(cmd, "worker"); worker != "" {
				values.Set("worker", worker)
			}

			values.Set("limit", strconv.Itoa(mustInt(cmd, "limit")))

			if cursor := mustString(cmd, "cursor"); cursor != "" {
				values.Set("cursor", cursor)
			}

			var page struct {
				Items      []processRow `json:"items"`
				NextCursor string       `json:"next_cursor"`
			}

			if err := newClient().do(cmd.Context(), "GET", query("/v1/processes", values), nil, &page); err != nil {
				return err
			}

			if mustString(cmd, "output") == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(page)
			}

			printProcessTable(cmd, page.Items)

			if page.NextCursor != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "more results: --cursor %s\n", page.NextCursor)
			}

			return nil
		},
	}

	cmd.Flags().StringSlice("status", nil, "filter by status, repeatable")
	cmd.Flags().String("worker", "", "filter by worker name")
	cmd.Flags().Int("limit", 50, "maximum number of rows")
	cmd.Flags().String("cursor", "", "continue a previous listing")
	cmd.Flags().String("output", "table", "output format: table or json")

	return cmd
}

func printProcessTable(cmd *cobra.Command, rows []processRow) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	defer w.Flush()

	fmt.Fprintln(w, "ID\tWORKER\tTYPE\tSTATUS\tPID\tATTEMPT\tEXIT")

	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			row.ID,
			row.Worker,
			row.Type,
			row.Status,
			formatOptionalInt(row.PID),
			row.Attempt,
			formatOptionalInt(row.ExitCode),
		)
	}
}

func formatOptionalInt(value *int) string {
	if value == nil {
		return "-"
	}

	return strconv.Itoa(*value)
}
