// Command processd runs the Processd daemon and its client CLI.
//
// All business logic lives in internal packages; main only delegates to the
// command tree so that exit-code handling stays in one place.
package main

import (
	"os"

	"github.com/curruwilla/processd/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
