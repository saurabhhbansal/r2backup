package schedule

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/winconsole"
)

// TestInstallAgainstTheRealScheduler registers a task with this machine's
// actual scheduler, reads it back, and removes it.
//
// Everything else in this package tests the generators -- the XML, the unit
// files, the cron line -- against strings, and all of it passed while
// `r2backup schedule --every 5` failed outright on a real Windows desktop
// with "exit status 1". Nothing anywhere ever called schtasks. A backup tool
// whose entire automation story is the OS scheduler has to prove it can
// actually register with one.
//
// Opt-in via R2BACKUP_TEST_SCHEDULER=1, because it mutates the machine it
// runs on: it registers a task, then removes it. CI sets it. It uses its own
// task name so it can never touch a real r2backup schedule, and removes that
// name first in case an earlier run was killed partway.
func TestInstallAgainstTheRealScheduler(t *testing.T) {
	if os.Getenv("R2BACKUP_TEST_SCHEDULER") != "1" {
		t.Skip("set R2BACKUP_TEST_SCHEDULER=1 to register a real scheduled task on this machine")
	}
	if !Supported() {
		t.Skipf("no scheduler implementation for %s", runtime.GOOS)
	}

	const name = "r2backup-selftest"
	binary := filepath.Join(t.TempDir(), "r2backup-selftest")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	// A real file: Task Scheduler validates that the action's command exists.
	if err := os.WriteFile(binary, []byte("not a real program"), 0o755); err != nil {
		t.Fatal(err)
	}

	_ = Remove(name) // in case a previous run was interrupted
	t.Cleanup(func() {
		if err := Remove(name); err != nil {
			t.Errorf("Remove(%s) during cleanup: %v", name, err)
		}
	})

	entry := Entry{
		Name:     name,
		Interval: 17 * time.Minute, // distinctive, so a stale task is obvious
		// The command line a real schedule is registered with, not a
		// simplified one: on Windows that includes --hidden, and a scheduler
		// that refused the argument would otherwise only be discovered by a
		// user whose backups had silently stopped running.
		BinaryPath: binary,
		Args:       []string{"backup", winconsole.HiddenFlag},
	}
	if err := Install(entry); err != nil {
		t.Fatalf("Install against the real scheduler: %v", err)
	}

	st, err := Current(name)
	if err != nil {
		t.Fatalf("Current(%s) after Install: %v", name, err)
	}
	if !st.Registered {
		t.Fatal("Install reported success but Current says nothing is registered")
	}
	// Interval is best-effort per platform, so it is only checked when the
	// platform managed to report one at all.
	if st.Interval != 0 && st.Interval != entry.Interval {
		t.Errorf("Interval = %s, want %s", st.Interval, entry.Interval)
	}
	t.Logf("registered on %s: interval=%s runs-when-signed-out=%v", runtime.GOOS, st.Interval, st.RunsWhenSignedOut)

	// On systemd, Registered and a parsed Interval both come from the unit
	// file this package wrote, so neither would have caught OnUnitActiveSec
	// alone leaving the timer with no trigger until its service had run once
	// (Persistent=true does nothing for it either: that only applies to
	// OnCalendar timers). NextElapseUSecMonotonic is systemd's own answer to
	// "when does this timer next fire," read from the live unit -- infinity
	// or empty means never, which is exactly the bug this asserts against.
	// systemdAvailable is Linux-only and unexported to this file's platform
	// reach, so availability is inferred the same way the rest of this check
	// has to: by asking systemctl directly and treating a failure to run it
	// (systemd absent, or Install fell back to cron) as nothing to assert.
	if runtime.GOOS == "linux" {
		unit := systemdUnitName(name) + ".timer"
		out, err := run(context.Background(), "systemctl", "--user", "show", unit, "-p", "NextElapseUSecMonotonic")
		if err != nil {
			t.Logf("systemctl --user show %s -p NextElapseUSecMonotonic unavailable, skipping: %v", unit, err)
		} else {
			next := strings.TrimSpace(parseSystemdShow(string(out))["NextElapseUSecMonotonic"])
			if next == "" || next == "infinity" {
				t.Errorf("NextElapseUSecMonotonic = %q, want a finite value -- the timer has no trigger and will never fire", next)
			}
		}
	}

	// Installing twice must overwrite rather than fail: `schedule --every N`
	// is how the interval is changed.
	entry.Interval = 23 * time.Minute
	if err := Install(entry); err != nil {
		t.Fatalf("second Install over the same name: %v", err)
	}
	if st, err := Current(name); err != nil {
		t.Fatalf("Current after the second Install: %v", err)
	} else if st.Interval != 0 && st.Interval != entry.Interval {
		t.Errorf("Interval after re-install = %s, want %s", st.Interval, entry.Interval)
	}

	if err := Remove(name); err != nil {
		t.Fatalf("Remove(%s): %v", name, err)
	}
	after, err := Current(name)
	if err != nil {
		t.Fatalf("Current(%s) after Remove: %v", name, err)
	}
	if after.Registered {
		t.Error("the task is still registered after Remove reported success")
	}
}
