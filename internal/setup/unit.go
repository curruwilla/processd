package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/curruwilla/processd/internal/config"
)

// unitStopMargin is added to shutdown_grace for TimeoutStopSec. A unit that
// times out first would kill the daemon while it is still stopping the process
// groups it supervises.
const unitStopMargin = 15 * time.Second

// systemDirs are the directories a service binary is expected to live in. A
// unit pointing anywhere else usually means the operator ran setup from a build
// tree, and the service breaks as soon as that tree moves.
var systemDirs = []string{"/usr/local/bin", "/usr/bin", "/usr/sbin", "/opt"}

// unitTemplate mirrors examples/processd.service. Every comment explains a
// value the daemon actually depends on, so keep both in sync.
const unitTemplate = `[Unit]
Description=Processd process manager
Documentation=https://github.com/curruwilla/processd
After=network.target

[Service]
Type=exec
ExecStart=%s serve --config %s
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5s

# Must exceed shutdown_grace, otherwise systemd kills the daemon in the middle
# of stopping the processes it supervises.
TimeoutStopSec=%s

# mixed: systemd signals the daemon only, leaving it to terminate the process
# groups it owns in the right order.
KillMode=mixed
KillSignal=SIGTERM

# Each supervised execution costs about five descriptors: two log files and both
# ends of the stdout and stderr pipes.
LimitNOFILE=524288

# The daemon needs privileges to drop them: it starts each process as the user
# the worker declares.
User=root
StateDirectory=processd
LogsDirectory=processd

NoNewPrivileges=false
ProtectSystem=full
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
`

// renderUnit builds the systemd unit for this installation.
func renderUnit(binary, configPath string, grace time.Duration) string {
	return fmt.Sprintf(unitTemplate, binary, configPath, grace+unitStopMargin)
}

// installService writes the unit and hands the service to systemd.
func (o Options) installService(ctx context.Context, cfg config.Config, res *Result) error {
	res.Service = o.Service
	res.UnitPath = o.UnitPath

	if !o.InstallUnit {
		res.ServiceState = "skipped"
		res.step("left systemd alone")

		return nil
	}

	if o.Systemctl == nil && !systemdRunning() {
		res.ServiceState = "unavailable"
		res.warn("systemd is not running here, so no service was installed; start the daemon with: processd serve --config " + o.ConfigPath)

		return nil
	}

	o.warnAboutBinary(res)

	if err := o.writeUnit(cfg, res); err != nil {
		return err
	}

	return o.enableService(ctx, res)
}

func (o Options) writeUnit(cfg config.Config, res *Result) error {
	unit := []byte(renderUnit(o.Binary, o.ConfigPath, cfg.ShutdownGrace.Duration()))

	current, err := os.ReadFile(o.UnitPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return permissionHint(fmt.Errorf("reading %s: %w", o.UnitPath, err))
	}

	if o.DryRun {
		res.step("would write " + o.UnitPath)

		return nil
	}

	if string(current) == string(unit) {
		res.step("unit already current: " + o.UnitPath)

		return nil
	}

	if err := os.MkdirAll(filepath.Dir(o.UnitPath), 0o755); err != nil { //nolint:gosec // /etc/systemd/system must stay world-readable
		return permissionHint(fmt.Errorf("creating %s: %w", filepath.Dir(o.UnitPath), err))
	}

	// Unit files are read by systemd itself and carry no secret.
	if err := os.WriteFile(o.UnitPath, unit, 0o644); err != nil { //nolint:gosec // a unit unreadable by systemd is useless
		return permissionHint(fmt.Errorf("writing %s: %w", o.UnitPath, err))
	}

	res.step("wrote " + o.UnitPath)

	return nil
}

func (o Options) enableService(ctx context.Context, res *Result) error {
	run := o.Systemctl
	if run == nil {
		run = execRunner
	}

	commands := [][]string{{"daemon-reload"}}

	if o.StartUnit {
		commands = append(commands, []string{"enable", "--now", o.Service})
		res.ServiceState = "started"
	} else {
		commands = append(commands, []string{"enable", o.Service})
		res.ServiceState = "enabled"
	}

	for _, args := range commands {
		if o.DryRun {
			res.step("would run systemctl " + strings.Join(args, " "))

			continue
		}

		if err := run(ctx, "systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}

		res.step("ran systemctl " + strings.Join(args, " "))
	}

	return nil
}

// warnAboutBinary flags a unit that would start a binary outside the usual
// system paths — a build tree that gets rebuilt or removed takes the service
// down with it.
func (o Options) warnAboutBinary(res *Result) {
	dir := filepath.Dir(o.Binary)

	if slices.ContainsFunc(systemDirs, func(prefix string) bool { return dir == prefix || strings.HasPrefix(dir, prefix+"/") }) {
		return
	}

	res.warn("the unit starts " + o.Binary + ", which is outside the system paths; install it first with: " +
		"sudo install -m 0755 " + o.Binary + " /usr/local/bin/processd")
}

// systemdRunning reports whether the host is actually running systemd. The
// directory only exists when systemd is pid 1.
func systemdRunning() bool {
	info, err := os.Stat("/run/systemd/system")

	return err == nil && info.IsDir()
}

// execRunner is the production Runner: it forwards stdout and stderr so that a
// systemctl failure explains itself.
func execRunner(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the arguments are built here, never by a client
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", name, err)
	}

	return nil
}
