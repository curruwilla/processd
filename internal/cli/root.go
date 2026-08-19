// Package cli implements the processd command tree. Every client command talks
// to the daemon through the public REST API — there is no private back door.
package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/curruwilla/processd/internal/version"
)

// Exit codes follow the usual Unix conventions.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// errUsage marks a failure caused by bad input rather than a runtime problem.
var errUsage = errors.New("usage error")

// Execute runs the command tree and returns the process exit code. main only
// forwards it, so cleanup and error rendering stay here.
func Execute() int {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)

		if errors.Is(err, errUsage) {
			return exitUsage
		}

		return exitError
	}

	return exitOK
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "processd",
		Short: "Run any CLI process through a simple API and keep it alive",
		Long: "Processd is a lightweight process manager that runs and supervises CLI\n" +
			"processes through a REST API.",
		Version: version.String(),
		// The version string already names the program; the default template
		// would print "processd version processd ...".
		// Cobra should not print usage on every runtime failure, and error
		// rendering belongs to Execute.
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: initConfig,
	}

	cmd.SetVersionTemplate("{{.Version}}\n")

	cmd.PersistentFlags().String("config", "/etc/processd/processd.yaml", "path to the daemon configuration")
	cmd.PersistentFlags().String("server", "http://127.0.0.1:7373", "base URL of the processd API")
	cmd.PersistentFlags().String("token", "", "API token")
	cmd.PersistentFlags().String("log-level", "info", "log level: debug, info, warn or error")

	cmd.AddCommand(
		newServeCommand(),
		newStatusCommand(),
		newPsCommand(),
		newRunCommand(),
		newStopCommand(),
		newSignalCommand(),
		newLogsCommand(),
		newWorkersCommand(),
		newReloadCommand(),
		newTokenCommand(),
	)

	return cmd
}

// initConfig wires flags to environment variables so that PROCESSD_TOKEN and
// friends work without repeating flags in scripts.
func initConfig(cmd *cobra.Command, _ []string) error {
	viper.SetEnvPrefix("PROCESSD")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	for _, name := range []string{"config", "server", "token", "log-level"} {
		if err := viper.BindPFlag(name, cmd.Root().PersistentFlags().Lookup(name)); err != nil {
			return fmt.Errorf("binding flag %q: %w", name, err)
		}
	}

	return nil
}

// newLogger builds the daemon logger. Logs go to stderr so that stdout stays a
// clean data stream for pipes.
func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parsed})

	return slog.New(handler)
}
