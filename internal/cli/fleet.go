package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// fleetNode is the subset of a node status the CLI prints.
type fleetNode struct {
	Name      string     `json:"name"`
	URL       string     `json:"url"`
	Reachable bool       `json:"reachable"`
	Error     string     `json:"error"`
	Version   string     `json:"version"`
	LastSeen  *time.Time `json:"last_seen"`
	Stats     struct {
		SlotsUsed  int `json:"slots_used"`
		SlotsMax   int `json:"slots_max"`
		Running    int `json:"running"`
		QueueDepth int `json:"queue_depth"`
	} `json:"stats"`
}

func newFleetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "List the nodes this hub reads",
		Long: "List the nodes this hub aggregates.\n\n" +
			"The view is read-only: a hub polls its nodes and proxies reads to them, and never " +
			"writes to one. A node that is unreachable degrades the view rather than the fleet.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var nodes []fleetNode

			if err := newClient().do(cmd.Context(), "GET", "/v1/fleet/nodes", nil, &nodes); err != nil {
				return err
			}

			if mustString(cmd, "output") == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(nodes)
			}

			printFleetTable(cmd, nodes)

			// The reasons go under the table, not in it: a dial error is longer
			// than every other column put together and would make the table
			// unreadable on the day it matters most.
			for _, node := range nodes {
				if !node.Reachable && node.Error != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", node.Name, node.Error)
				}
			}

			return nil
		},
	}

	cmd.Flags().String("output", "table", "output format: table or json")

	return cmd
}

func printFleetTable(cmd *cobra.Command, nodes []fleetNode) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	defer w.Flush()

	fmt.Fprintln(w, "NODE\tREACHABLE\tVERSION\tSLOTS\tRUNNING\tQUEUED\tLAST SEEN\tURL")

	for _, node := range nodes {
		lastSeen := "never"
		if node.LastSeen != nil {
			lastSeen = node.LastSeen.Local().Format(time.RFC3339)
		}

		reachable := "yes"
		if !node.Reachable {
			reachable = "no"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%d\t%d\t%s\t%s\n",
			node.Name,
			reachable,
			orDash(node.Version),
			node.Stats.SlotsUsed, node.Stats.SlotsMax,
			node.Stats.Running,
			node.Stats.QueueDepth,
			lastSeen,
			node.URL,
		)
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}

	return value
}
