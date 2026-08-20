package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/curruwilla/processd/internal/setup"
)

func TestPrintSetupReportsEverythingNeededLater(t *testing.T) {
	t.Parallel()

	result := setup.Result{
		ConfigPath: "/etc/processd/processd.yaml",
		TokenPath:  "/etc/processd/token",
		TokenName:  "admin",
		Token:      "0123456789abcdef",
		WorkersDir: "/etc/processd/workers.d",
		DataDir:    "/var/lib/processd",
		LogDir:     "/var/log/processd",
		BaseURL:    "http://127.0.0.1:7373",
		UIURL:      "http://127.0.0.1:7373/ui/",
		MetricsURL: "http://127.0.0.1:7373/v1/metrics",
		UnitPath:   "/etc/systemd/system/processd.service",
		Service:    "processd",

		ServiceState: "started",
		Steps:        []string{"wrote /etc/processd/processd.yaml"},
		Warnings:     []string{"install the binary first"},
	}

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	printSetup(cmd, result)

	// Everything an operator needs to check the node without reading the code.
	wants := []string{
		result.Token,
		result.ConfigPath,
		result.TokenPath,
		result.WorkersDir,
		result.DataDir,
		result.LogDir,
		result.UIURL,
		result.MetricsURL,
		result.UnitPath,
		"systemctl status processd",
		"journalctl -u processd -f",
		"install the binary first",
	}

	for _, want := range wants {
		if !strings.Contains(out.String(), want) {
			t.Errorf("printSetup() output is missing %q:\n%s", want, out)
		}
	}
}

func TestPrintSetupMarksADryRun(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	printSetup(cmd, setup.Result{DryRun: true, ServiceState: "skipped", Steps: []string{"would write /etc/processd/processd.yaml"}})

	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("printSetup() did not mark the dry run:\n%s", out)
	}

	// A skipped service must not advertise systemctl commands that do nothing.
	if strings.Contains(out.String(), "systemctl status") {
		t.Errorf("printSetup() suggested systemctl for a node without a unit:\n%s", out)
	}
}

func TestSetupCommandFlags(t *testing.T) {
	t.Parallel()

	cmd := newSetupCommand()

	for _, name := range []string{"listen", "token-name", "binary", "unit", "service", "systemd", "start", "rotate-token", "dry-run", "output"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("setup command has no --%s flag", name)
		}
	}
}
