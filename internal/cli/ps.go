package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// processRow is the subset of the process representation the CLI prints.
type processRow struct {
	// Node is empty unless the row came from a fleet listing.
	Node     string `json:"node"`
	ID       string `json:"id"`
	Worker   string `json:"worker"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	PID      *int   `json:"pid"`
	Attempt  int    `json:"attempt"`
	Restarts int    `json:"restarts"`
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

			if kind := mustString(cmd, "type"); kind != "" {
				values.Set("type", kind)
			}

			// On a hub, naming a node reads that node instead of this one. "*"
			// merges every node into one page.
			node := mustString(cmd, "node")
			if node != "" {
				values.Set("node", node)
			}

			values.Set("limit", strconv.Itoa(mustInt(cmd, "limit")))

			if cursor := mustString(cmd, "cursor"); cursor != "" {
				values.Set("cursor", cursor)
			}

			var page struct {
				Items       []processRow `json:"items"`
				NextCursor  string       `json:"next_cursor"`
				Unreachable []string     `json:"unreachable"`
			}

			if err := newClient().do(cmd.Context(), "GET", query("/v1/processes", values), nil, &page); err != nil {
				return err
			}

			if mustString(cmd, "output") == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(page)
			}

			printProcessTable(cmd, page.Items, node != "")

			// A degraded page has to say so: rows that are missing because a
			// node did not answer look exactly like rows that do not exist.
			if len(page.Unreachable) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "unreachable nodes: %s\n", strings.Join(page.Unreachable, ", "))
			}

			if page.NextCursor != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "more results: --cursor %s\n", page.NextCursor)
			}

			return nil
		},
	}

	cmd.Flags().StringSlice("status", nil, "filter by status, repeatable")
	cmd.Flags().String("worker", "", "filter by worker name")
	cmd.Flags().String("type", "", "filter by execution type: task or service")
	cmd.Flags().Int("limit", 50, "maximum number of rows")
	cmd.Flags().String("cursor", "", "continue a previous listing")
	cmd.Flags().String("output", "table", "output format: table or json")
	cmd.Flags().String("node", "", `read a fleet node instead of this one, or "*" to merge them all`)

	return cmd
}

// printProcessTable renders the listing. The node column appears only for a
// fleet listing, where an ID on its own cannot be looked up again.
func printProcessTable(cmd *cobra.Command, rows []processRow, withNode bool) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	defer w.Flush()

	header := "ID\tWORKER\tTYPE\tSTATUS\tPID\tATTEMPT\tRESTARTS\tEXIT"
	if withNode {
		header = "NODE\t" + header
	}

	fmt.Fprintln(w, header)

	for _, row := range rows {
		if withNode {
			fmt.Fprintf(w, "%s\t", orDash(row.Node))
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			row.ID,
			row.Worker,
			row.Type,
			row.Status,
			formatOptionalInt(row.PID),
			row.Attempt,
			row.Restarts,
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
