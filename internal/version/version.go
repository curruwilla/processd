// Package version exposes build metadata injected at link time.
package version

import "fmt"

// Values are overridden with -ldflags at build time. See the Makefile.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a single-line, human-readable build identifier.
func String() string {
	return fmt.Sprintf("processd %s (commit %s, built %s)", Version, Commit, Date)
}
