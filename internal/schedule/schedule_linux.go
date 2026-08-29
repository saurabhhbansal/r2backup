//go:build linux

package schedule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Supported reports whether this package can register scheduled runs.
// Linux always has cron as a fallback even without systemd, so it is always
// true.
func Supported() bool { return true }

// Install registers e as a systemd user timer, or as a crontab entry if the
// systemd user instance is not available (containers, some minimal distros).
// Calling Install twice with the same Name is a no-op on the entry it
// already made plus an update of anything that changed (notably the
// interval), never a second registration.
func Install(e Entry) error {
	if err := e.validate(); err != nil {
		return err
	}
	if systemdAvailable() {
		return installSystemd(e)
	}
	return installCron(e)
}

// Remove deletes whatever Install registered under name, whether that was a
// systemd timer or a crontab entry -- both are attempted, since which one
// Install used can differ from what's available "now" (e.g. systemd was
// installed after r2backup was first scheduled). Calling Remove when
// nothing is registered is not an error.
func Remove(name string) error {
	if err := removeSystemd(name); err != nil {
		return err
	}
	return removeCron(name)
}

// Current reports what is currently registered for name: a systemd timer is
// checked first (by reading the unit file this package itself wrote, which
// is what makes Interval reliable), then the crontab fallback.
func Current(name string) (Status, error) {
	dir, dirErr := systemdUserDir()
	if dirErr == nil {
		unit := systemdUnitName(name)
		timerPath := filepath.Join(dir, unit+".timer")
		if data, err := os.ReadFile(timerPath); err == nil {
			st := Status{Registered: true}
			if iv, perr := parseSystemdTimerInterval(string(data)); perr == nil {
				st.Interval = iv
			}
			ctx := context.Background()
			if out, err := run(ctx, "systemctl", "--user", "show", unit+".timer",
				"-p", "ActiveState", "-p", "LastTriggerUSec", "-p", "NextElapseUSecRealtime"); err == nil {
				props := parseSystemdShow(string(out))
				st.LastResult = props["ActiveState"]
				st.LastRun = parseSystemdTimestamp(props["LastTriggerUSec"])
				st.NextRun = parseSystemdTimestamp(props["NextElapseUSecRealtime"])
			}
			return st, nil
		}
	}
	ctx := context.Background()
	lines, err := readCrontab(ctx)
	if err != nil {
		return Status{}, err
	}
	marker := cronMarker(name)
	for _, l := range lines {
		if strings.Contains(l, marker) {
			return Status{Registered: true}, nil
		}
	}
	return Status{}, nil
}

// systemdAvailable detects whether this session can use `systemctl --user`
// at all, rather than assuming it: some distros and most containers don't
// ship systemd as PID 1, in which case even a present systemctl binary has
// no user manager to talk to.
func systemdAvailable() bool {
	if _, err := lookPath("systemctl"); err != nil {
		return false
	}
	ctx := context.Background()
	_, err := run(ctx, "systemctl", "--user", "status")
	// "status" with no unit still needs a reachable user manager to answer
	// at all; anything other than a hard failure to run it counts as
	// available (a non-zero exit for e.g. "degraded" state is fine).
	return err == nil || isExitError(err)
}

// systemdUserDir is where a per-user systemd unit belongs: no root, and it
// runs as the user who owns the files being backed up.
func systemdUserDir() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("schedule: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func installSystemd(e Entry) error {
	dir, err := systemdUserDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("schedule: create systemd user directory: %w", err)
	}
	unit := systemdUnitName(e.Name)
	servicePath := filepath.Join(dir, unit+".service")
	timerPath := filepath.Join(dir, unit+".timer")
	if err := os.WriteFile(servicePath, []byte(systemdServiceUnit(e)), 0o600); err != nil {
		return fmt.Errorf("schedule: write %s: %w", servicePath, err)
	}
	if err := os.WriteFile(timerPath, []byte(systemdTimerUnit(e)), 0o600); err != nil {
		return fmt.Errorf("schedule: write %s: %w", timerPath, err)
	}
	ctx := context.Background()
	if out, err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return cmdError("schedule: systemctl --user daemon-reload", out, err)
	}
	if out, err := run(ctx, "systemctl", "--user", "enable", unit+".timer"); err != nil {
		return cmdError("schedule: systemctl --user enable "+unit+".timer", out, err)
	}
	// restart, not start: picks up a changed OnUnitActiveSec even when the
	// timer was already running from a previous Install. This -- plus
	// overwriting the same unit files above -- is what makes Install
	// idempotent for an interval change, not just a repeat of the same one.
	if out, err := run(ctx, "systemctl", "--user", "restart", unit+".timer"); err != nil {
		return cmdError("schedule: systemctl --user restart "+unit+".timer", out, err)
	}
	return nil
}

func removeSystemd(name string) error {
	dir, err := systemdUserDir()
	if err != nil {
		return err
	}
	unit := systemdUnitName(name)
	timerPath := filepath.Join(dir, unit+".timer")
	servicePath := filepath.Join(dir, unit+".service")
	if _, err := os.Stat(timerPath); err != nil {
		// Nothing this package registered under this name -- not an error.
		return nil
	}
	ctx := context.Background()
	// Best effort: an already-stopped or already-disabled timer errors here
	// on some systemd versions, and either way the file removal below is
	// what actually makes it gone for good.
	_, _ = run(ctx, "systemctl", "--user", "disable", "--now", unit+".timer")
	if err := os.Remove(timerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("schedule: remove %s: %w", timerPath, err)
	}
	if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("schedule: remove %s: %w", servicePath, err)
	}
	_, _ = run(ctx, "systemctl", "--user", "daemon-reload")
	return nil
}

func installCron(e Entry) error {
	entries, err := cronEntries(e)
	if err != nil {
		return err
	}
	ctx := context.Background()
	lines, err := readCrontab(ctx)
	if err != nil {
		return err
	}
	marker := cronMarker(e.Name)
	kept := make([]string, 0, len(lines)+len(entries))
	for _, l := range lines {
		if !strings.Contains(l, marker) {
			kept = append(kept, l)
		}
	}
	kept = append(kept, entries...)
	return writeCrontab(ctx, kept)
}

func removeCron(name string) error {
	ctx := context.Background()
	lines, err := readCrontab(ctx)
	if err != nil {
		return err
	}
	marker := cronMarker(name)
	kept := make([]string, 0, len(lines))
	changed := false
	for _, l := range lines {
		if strings.Contains(l, marker) {
			changed = true
			continue
		}
		kept = append(kept, l)
	}
	if !changed {
		return nil
	}
	return writeCrontab(ctx, kept)
}

// readCrontab returns the current user's crontab as individual lines, or
// nil if they have none at all.
func readCrontab(ctx context.Context) ([]string, error) {
	out, err := run(ctx, "crontab", "-l")
	if err != nil {
		// Every cron implementation we target exits non-zero for "no
		// crontab for user" -- that is an empty crontab, not a failure.
		if strings.Contains(strings.ToLower(string(out)), "no crontab") {
			return nil, nil
		}
		// Nor is cron not being installed at all. A machine with no crontab
		// binary certainly has no cron entry of ours to find or remove, which
		// is the same answer as an empty crontab. Reporting it as an error
		// made `schedule --remove` fail on a systemd-only box that had never
		// used cron in the first place.
		if errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("schedule: crontab -l: %w", err)
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// writeCrontab replaces the current user's crontab with lines. It goes
// through a temp file and `crontab <file>` rather than piping to
// `crontab -`'s stdin, because Runner only carries argv -- and passing a
// filename is exactly what crontab(1) supports for this.
func writeCrontab(ctx context.Context, lines []string) error {
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	tmp, err := os.CreateTemp("", "r2backup-crontab-*")
	if err != nil {
		return fmt.Errorf("schedule: create temp crontab file: %w", err)
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("schedule: write temp crontab file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("schedule: close temp crontab file: %w", err)
	}
	if out, err := run(ctx, "crontab", path); err != nil {
		return cmdError("schedule: crontab "+path, out, err)
	}
	return nil
}

// isExitError reports whether err is a command that ran and exited non-zero
// (as opposed to one that couldn't be started at all, e.g. binary missing).
// defaultRunner returns *exec.ExitError for the former; a fake test Runner
// can return any error, so this only special-cases the one defaultRunner
// actually produces.
func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}
