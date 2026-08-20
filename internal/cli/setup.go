package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/curruwilla/processd/internal/setup"
)

func newSetupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install this node: directories, configuration, API token and systemd unit",
		Long: "Prepares a machine to run processd in one step: creates the state\n" +
			"directories, writes the daemon configuration, mints an API token, and\n" +
			"installs and starts the systemd unit. It then prints the token and every\n" +
			"path and address needed to check the node later.\n" +
			"\n" +
			"Running it again is safe: the token already installed is kept, so clients\n" +
			"configured earlier keep working. Use --rotate-token to replace it.\n" +
			"\n" +
			"The configuration is rewritten from the values the daemon parsed, which\n" +
			"drops comments and reorders keys; the previous file is saved next to it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := setup.Run(cmd.Context(), setup.Options{
				ConfigPath:  viper.GetString("config"),
				Listen:      mustString(cmd, "listen"),
				TokenName:   mustString(cmd, "token-name"),
				Binary:      mustString(cmd, "binary"),
				UnitPath:    mustString(cmd, "unit"),
				Service:     mustString(cmd, "service"),
				InstallUnit: mustBool(cmd, "systemd"),
				StartUnit:   mustBool(cmd, "start"),
				Rotate:      mustBool(cmd, "rotate-token"),
				DryRun:      mustBool(cmd, "dry-run"),
			})
			if err != nil {
				return err
			}

			if mustString(cmd, "output") == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}

			printSetup(cmd, result)

			return nil
		},
	}

	cmd.Flags().String("listen", "", "address the daemon binds to (default: keep the configured value)")
	cmd.Flags().String("token-name", setup.DefaultTokenName, "name of the token in the configuration and the audit trail")
	cmd.Flags().String("binary", "", "processd path used by the unit (default: the running binary)")
	cmd.Flags().String("unit", "", "systemd unit path (default: /etc/systemd/system/<service>.service)")
	cmd.Flags().String("service", setup.DefaultService, "systemd service name")
	cmd.Flags().Bool("systemd", true, "install the systemd unit")
	cmd.Flags().Bool("start", true, "enable and start the service")
	cmd.Flags().Bool("rotate-token", false, "replace the token even when the current one still works")
	cmd.Flags().Bool("dry-run", false, "report what would be done without changing anything")
	cmd.Flags().String("output", "text", "output format: text or json")

	return cmd
}

// printSetup renders the node summary: what changed, where everything lives,
// and the commands to check it.
func printSetup(cmd *cobra.Command, r setup.Result) {
	out := cmd.OutOrStdout()

	if r.DryRun {
		fmt.Fprintln(out, "dry run: nothing was changed")
	}

	for _, step := range r.Steps {
		fmt.Fprintln(out, "  -", step)
	}

	fmt.Fprintln(out)
	printSetupPaths(cmd, r)
	printSetupToken(cmd, r)
	printSetupChecks(cmd, r)

	for _, warning := range r.Warnings {
		fmt.Fprintln(out, "warning:", warning)
	}
}

func printSetupPaths(cmd *cobra.Command, r setup.Result) {
	rows := [][2]string{
		{"config", r.ConfigPath},
		{"token file", r.TokenPath},
		{"workers", r.WorkersDir},
		{"data", r.DataDir},
		{"logs", r.LogDir},
		{"api", r.BaseURL},
	}

	if r.UIURL != "" {
		rows = append(rows, [2]string{"web ui", r.UIURL})
	}

	rows = append(rows, [2]string{"metrics", r.MetricsURL})

	if r.ConfigBackup != "" {
		rows = append(rows, [2]string{"backup", r.ConfigBackup})
	}

	if r.UnitPath != "" && r.ServiceState != "skipped" {
		rows = append(rows, [2]string{"unit", r.UnitPath}, [2]string{"service", r.Service + " (" + r.ServiceState + ")"})
	}

	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	defer writer.Flush()

	for _, row := range rows {
		fmt.Fprintf(writer, "%s\t%s\n", row[0], row[1])
	}
}

func printSetupToken(cmd *cobra.Command, r setup.Result) {
	out := cmd.OutOrStdout()

	origin := "new token"
	if r.TokenReused {
		origin = "existing token, unchanged"
	}

	fmt.Fprintf(out, "\ntoken %q (%s)\n", r.TokenName, origin)
	fmt.Fprintf(out, "  %s\n", r.Token)
	fmt.Fprintf(out, "  Paste this into the web UI, or export PROCESSD_TOKEN. The daemon keeps\n")
	fmt.Fprintf(out, "  only its sha256 digest: %s holds the only copy.\n", r.TokenPath)
}

func printSetupChecks(cmd *cobra.Command, r setup.Result) {
	out := cmd.OutOrStdout()

	fmt.Fprintln(out, "\ncheck")

	if r.Service != "" && r.ServiceState != "skipped" && r.ServiceState != "unavailable" {
		fmt.Fprintf(out, "  systemctl status %s\n", r.Service)
		fmt.Fprintf(out, "  journalctl -u %s -f\n", r.Service)
	}

	fmt.Fprintf(out, "  processd status --server %s\n", r.BaseURL)
	fmt.Fprintf(out, "  curl -sS %s/v1/health\n", r.BaseURL)
	fmt.Fprintf(out, "  curl -sS -H \"Authorization: Bearer $(sudo cat %s)\" %s/v1/workers\n", r.TokenPath, r.BaseURL)
	fmt.Fprintf(out, "  worker definitions go in %s, then: processd reload\n", r.WorkersDir)
}
