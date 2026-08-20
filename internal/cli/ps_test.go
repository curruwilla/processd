package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestFormatOptionalInt(t *testing.T) {
	t.Parallel()

	value := 42

	if got := formatOptionalInt(&value); got != "42" {
		t.Errorf("formatOptionalInt(42) = %q, want \"42\"", got)
	}

	// A queued execution has no pid and no exit code yet; printing 0 would read
	// as a successful exit.
	if got := formatOptionalInt(nil); got != "-" {
		t.Errorf("formatOptionalInt(nil) = %q, want \"-\"", got)
	}
}

func TestPrintProcessTable(t *testing.T) {
	t.Parallel()

	pid := 4242
	exit := 0

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	printProcessTable(cmd, []processRow{
		{ID: "proc_1", Worker: "invoice", Status: "COMPLETED", PID: &pid, Attempt: 1, ExitCode: &exit},
		{ID: "proc_2", Worker: "invoice", Status: "QUEUED"},
	})

	rendered := out.String()

	for _, want := range []string{"ID", "proc_1", "COMPLETED", "4242", "proc_2", "QUEUED"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("table is missing %q:\n%s", want, rendered)
		}
	}

	if !strings.Contains(rendered, "-") {
		t.Errorf("queued row has no placeholder for the missing pid:\n%s", rendered)
	}
}
