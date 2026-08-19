package cli

import "github.com/spf13/cobra"

// The mustX helpers read flags that are always declared by the command that
// uses them: a lookup failure would be a programming error, not user input.

func mustString(cmd *cobra.Command, name string) string {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		panic(err)
	}

	return value
}

func mustStringSlice(cmd *cobra.Command, name string) []string {
	value, err := cmd.Flags().GetStringSlice(name)
	if err != nil {
		panic(err)
	}

	return value
}

func mustInt(cmd *cobra.Command, name string) int {
	value, err := cmd.Flags().GetInt(name)
	if err != nil {
		panic(err)
	}

	return value
}
