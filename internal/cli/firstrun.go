package cli

import (
	"os"
	"runtime"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
	"github.com/saurabhhbansal/r2backup/internal/tray"
)

// A new setup starts with the two things most people want and forget to turn
// on: backing up on its own, and r2backup starting with Windows. They are
// only ever switched on here, the first time this computer gets storage --
// never for a computer that already had it, so nobody's saved choice flips.

// firstRunDefaults is a variable so tests can keep it away from the real Task
// Scheduler and registry.
var firstRunDefaults = applyFirstRunDefaults

// startAtLogin is the same kind of seam for the sign-in entry.
var startAtLogin = tray.SetStartAtLogin

// saveCreds stores the main storage keys and, if this computer had none
// before, switches on the first-setup defaults.
func (a *app) saveCreds(c creds.Credentials) error {
	had := a.creds.Exists()
	if err := a.creds.Save(c); err != nil {
		return err
	}
	if !had {
		firstRunDefaults()
	}
	return nil
}

// applyFirstRunDefaults is best effort: failing to schedule must not fail
// setup, and the panel's Settings shows the truth if it did not take.
func applyFirstRunDefaults() {
	if schedule.Supported() {
		if st, err := scheduleCurrent(scheduleName()); err == nil && !st.Registered {
			if self, err := os.Executable(); err == nil {
				binary, _ := scheduledBinary(runtime.GOOS, self, fileExists)
				_ = scheduleInstall(schedule.Entry{
					Name:       scheduleName(),
					Interval:   time.Duration(schedule.DefaultIntervalMinutes) * time.Minute,
					BinaryPath: binary,
					Args:       scheduledRunArgs(runtime.GOOS, config.DataDirArgs()),
				})
			}
		}
	}
	if runtime.GOOS == "windows" {
		_ = startAtLogin(true)
	}
}
