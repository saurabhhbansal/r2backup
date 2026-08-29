//go:build darwin

package schedule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/saurabhhbansal/r2backup/internal/config"
)

// Supported reports whether this package can register scheduled runs.
// launchd is always present on macOS.
func Supported() bool { return true }

// Install registers e as a LaunchAgent. Calling Install twice with the same
// Name rewrites the plist and reloads it -- bootout before bootstrap -- so
// an interval change takes effect rather than being ignored by an
// already-loaded agent.
func Install(e Entry) error {
	if err := e.validate(); err != nil {
		return err
	}
	dir, err := launchAgentsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("schedule: create LaunchAgents directory: %w", err)
	}
	stdout, stderr, err := logPaths(e.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(stdout), 0o700); err != nil {
		return fmt.Errorf("schedule: create log directory: %w", err)
	}
	content, err := launchdPlist(e, stdout, stderr)
	if err != nil {
		return fmt.Errorf("schedule: build plist: %w", err)
	}
	plistPath := filepath.Join(dir, launchdLabel(e.Name)+".plist")
	if err := os.WriteFile(plistPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("schedule: write %s: %w", plistPath, err)
	}
	ctx := context.Background()
	target := guiTarget()
	// bootout before bootstrap: bootstrap fails outright if the label is
	// already loaded, and a changed StartInterval only takes effect on a
	// fresh load -- this pair is what makes Install idempotent for an
	// interval change, not just a repeat of the same one. The bootout error
	// is ignored: it fails harmlessly when nothing was loaded yet.
	_, _ = run(ctx, "launchctl", "bootout", target, plistPath)
	if _, err := run(ctx, "launchctl", "bootstrap", target, plistPath); err != nil {
		// launchctl bootstrap/bootout arrived in 10.11; older systems only
		// have load/unload.
		if _, err2 := run(ctx, "launchctl", "load", "-w", plistPath); err2 != nil {
			return fmt.Errorf("schedule: launchctl bootstrap %s: %w (load fallback: %v)", plistPath, err, err2)
		}
	}
	return nil
}

// Remove unloads and deletes the LaunchAgent registered under name. Not
// being registered at all is not an error.
func Remove(name string) error {
	dir, err := launchAgentsDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(dir, launchdLabel(name)+".plist")
	if _, statErr := os.Stat(plistPath); statErr != nil {
		return nil
	}
	ctx := context.Background()
	target := guiTarget()
	// Best effort across both APIs, then remove the file regardless: an
	// already-unloaded agent errors here on some macOS versions, and the
	// file removal is what actually makes it gone for good.
	_, _ = run(ctx, "launchctl", "bootout", target, plistPath)
	_, _ = run(ctx, "launchctl", "unload", plistPath)
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("schedule: remove %s: %w", plistPath, err)
	}
	return nil
}

// Current reports what is currently registered for name, read back from the
// plist this package itself wrote.
func Current(name string) (Status, error) {
	dir, err := launchAgentsDir()
	if err != nil {
		return Status{}, err
	}
	label := launchdLabel(name)
	plistPath := filepath.Join(dir, label+".plist")
	data, err := os.ReadFile(plistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Status{}, nil
		}
		return Status{}, fmt.Errorf("schedule: read %s: %w", plistPath, err)
	}
	st := Status{Registered: true}
	if iv, perr := launchdParseStartInterval(string(data)); perr == nil {
		st.Interval = iv
	}
	ctx := context.Background()
	if out, err := run(ctx, "launchctl", "list", label); err == nil {
		st.LastResult = parseLaunchctlListStatus(string(out))
	}
	return st, nil
}

// launchAgentsDir is where a per-user LaunchAgent belongs: no root, and it
// runs as the user who owns the files being backed up.
func launchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("schedule: locate home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// logPaths is where StandardOutPath/StandardErrorPath point, since launchd
// gives a LaunchAgent no console: a failing scheduled run would otherwise
// leave no trace anywhere. Routed through config.DataDir so these logs land
// next to the index, run history and progress file r2backup already keeps
// there, and so R2BACKUP_DATA_DIR redirects them the same way it redirects
// everything else in tests.
func logPaths(name string) (stdout, stderr string, err error) {
	dataDir, derr := config.DataDir()
	if derr != nil {
		return "", "", fmt.Errorf("schedule: locate data directory: %w", derr)
	}
	base := filepath.Join(dataDir, "logs")
	unit := sanitizeUnitName(name)
	return filepath.Join(base, unit+".out.log"), filepath.Join(base, unit+".err.log"), nil
}

// guiTarget is the launchctl bootstrap/bootout domain for this user's GUI
// session.
func guiTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}
