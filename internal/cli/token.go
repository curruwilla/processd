package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/curruwilla/processd/internal/api"
)

func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Work with API tokens",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "hash",
		Short: "Print the configuration digest of a token read from stdin",
		Long: "Reads a token from stdin and prints the value to store in\n" +
			"auth.tokens[].hash. The token is read from stdin rather than from a\n" +
			"flag so that it never appears in the process list or shell history.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reader := bufio.NewReader(cmd.InOrStdin())

			line, err := reader.ReadString('\n')
			if err != nil && line == "" {
				return fmt.Errorf("reading token from stdin: %w", err)
			}

			token := strings.TrimSpace(line)
			if token == "" {
				return fmt.Errorf("%w: no token on stdin", errUsage)
			}

			fmt.Fprintln(cmd.OutOrStdout(), api.HashToken(token))

			return nil
		},
	})

	return cmd
}
