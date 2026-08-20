package cli

import (
	"encoding/json"
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

			if mustBool(cmd, "follow") {
				return followLogs(cmd, args[0], values)
			}

			return printLogs(cmd, args[0], values)
		},
	}

	cmd.Flags().String("stream", "both", "stream to read: stdout, stderr or both")
	cmd.Flags().Int("attempt", 0, "attempt to read, defaults to the last one")
	cmd.Flags().Int("tail", 0, "show only the last N lines")
	cmd.Flags().BoolP("follow", "f", false, "keep printing new output until the attempt ends")

	return cmd
}

// printLogs writes what the attempt has already produced.
func printLogs(cmd *cobra.Command, id string, values url.Values) error {
	path := query("/v1/processes/"+url.PathEscape(id)+"/logs", values)

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
}

// followLogs streams the attempt until it ends, or until the user interrupts.
//
// Captured stderr is written to stderr, so that piping the command keeps the
// same separation the execution itself had.
func followLogs(cmd *cobra.Command, id string, values url.Values) error {
	path := query("/v1/processes/"+url.PathEscape(id)+"/logs/stream", values)

	return newClient().stream(cmd.Context(), path, func(event, data string) error {
		switch event {
		case "line":
			var line struct {
				Stream string `json:"stream"`
				Text   string `json:"text"`
			}

			if err := json.Unmarshal([]byte(data), &line); err != nil {
				return fmt.Errorf("decoding streamed line: %w", err)
			}

			out := cmd.OutOrStdout()
			if line.Stream == "stderr" {
				out = cmd.ErrOrStderr()
			}

			fmt.Fprintln(out, line.Text)
		case "end":
			var end struct {
				Attempt   int    `json:"attempt"`
				Status    string `json:"status"`
				Truncated bool   `json:"truncated"`
			}

			if err := json.Unmarshal([]byte(data), &end); err != nil {
				return fmt.Errorf("decoding stream end: %w", err)
			}

			if end.Truncated {
				fmt.Fprintln(cmd.ErrOrStderr(), "output was truncated at the configured size cap")
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "attempt %d ended: %s\n", end.Attempt, end.Status)
		}

		return nil
	})
}
