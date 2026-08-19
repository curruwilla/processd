package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/daemon"
)

func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the processd daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := viper.GetString("config")

			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			log := newLogger(viper.GetString("log-level"))

			d, err := daemon.New(cfg, log)
			if err != nil {
				return err
			}

			defer func() {
				if closeErr := d.Close(); closeErr != nil {
					log.Error("closing daemon", "error", closeErr)
				}
			}()

			if err := d.Run(cmd.Context()); err != nil {
				return fmt.Errorf("running daemon: %w", err)
			}

			return nil
		},
	}
}
