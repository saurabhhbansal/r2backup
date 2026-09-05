package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// settingsFileName holds the handful of choices that belong to this machine
// rather than to a set.
//
// It is its own file rather than a key in the index, and the spending limit is
// why. The index is rebuildable state -- delete it and the next run works out
// what to upload again -- so a limit stored there would quietly disappear on a
// rebuild, and backups would resume spending without the ceiling their owner
// set. A setting that fails open is worse than no setting, so it lives
// somewhere nothing else has a reason to delete.
const settingsFileName = "settings.json"

// Settings are the per-machine choices.
//
// Every field is optional and the zero value is the shipped default, so a
// missing file, an empty file and a file written by an older build all mean
// the same thing: nothing has been chosen yet.
type Settings struct {
	// BudgetUSD is the monthly spending limit in US dollars. Zero means no
	// limit, which is the default and the only safe one -- see
	// internal/cost.Budget for why this is never set for someone.
	BudgetUSD float64 `json:"budget_usd,omitempty"`

	// BudgetResumedMonth is the calendar month ("2006-01", UTC) someone
	// chose to carry on backing up in after the limit was reached.
	//
	// Stored as a month rather than a flag so that carrying on expires by
	// itself. See cost.Budget.ResumedMonth for the argument.
	BudgetResumedMonth string `json:"budget_resumed_month,omitempty"`

	// LastUpdateCheck is when this machine last asked whether a newer
	// release exists, as a Unix timestamp. It throttles the check that runs
	// after a command finishes -- see internal/cli.
	//
	// Zero means never asked, which is what a fresh install looks like and
	// which correctly triggers a check on the first command.
	LastUpdateCheck int64 `json:"last_update_check,omitempty"`
}

// SettingsPath is where the settings file lives.
func SettingsPath() (string, error) { return sub(settingsFileName) }

// LoadSettings reads the settings for this machine.
//
// A missing file is not an error; it is what every fresh install looks like,
// and it reads back as the zero Settings. A corrupt file is an error, because
// silently treating unreadable settings as "no limit set" is precisely the
// fail-open behaviour the file exists to avoid.
func LoadSettings() (Settings, error) {
	path, err := SettingsPath()
	if err != nil {
		return Settings{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Settings{}, nil
		}
		return Settings{}, fmt.Errorf("read settings from %q: %w", path, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("settings at %q are not readable: %w", path, err)
	}
	return s, nil
}

// SaveSettings writes the settings for this machine.
//
// It writes to a temporary file and renames, so a crash mid-write leaves the
// previous settings intact rather than a truncated file that LoadSettings
// would refuse. The same reasoning as the progress file's atomic rename, for
// the same reason: a partially written file is worse than an old one.
func SaveSettings(s Settings) error {
	dir, err := EnsureDataDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, settingsFileName)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, settingsFileName+".*")
	if err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below has succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	// 0600 to match the rest of the data directory. Nothing here is secret,
	// but nothing here is another user's business either.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}
