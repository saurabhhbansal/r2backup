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
// It is its own file rather than a key in the index because the index is
// rebuildable state -- delete it and the next run works out what to upload
// again -- and a choice someone made should not disappear with a rebuild.
const settingsFileName = "settings.json"

// Settings are the per-machine choices.
//
// Every field is optional and the zero value is the shipped default, so a
// missing file, an empty file and a file written by an older build all mean
// the same thing: nothing has been chosen yet.
type Settings struct {
	// Older versions also kept a monthly spending limit here (budget_usd,
	// budget_resumed_month). There is no limit any more; those keys load
	// without error, are ignored, and are dropped on the next save.

	// LastUpdateCheck is when this machine last asked whether a newer
	// release exists, as a Unix timestamp. It throttles the check that runs
	// after a command finishes -- see internal/cli.
	//
	// Zero means never asked, which is what a fresh install looks like and
	// which correctly triggers a check on the first command.
	LastUpdateCheck int64 `json:"last_update_check,omitempty"`

	// MirrorAuto copies each set to the mirror destination (see `r2b
	// mirror`) after it is backed up. Off until someone turns it on.
	MirrorAuto bool `json:"mirror_auto,omitempty"`
	// MirrorLast is when each set was last mirrored in full, as Unix
	// seconds, so an automatic mirror can skip a set that has not changed
	// since -- and still run once a day to catch anything a failed run left.
	MirrorLast map[string]int64 `json:"mirror_last,omitempty"`

	// MachineName is what this computer is called, chosen in the panel.
	// Empty means the computer's own hostname. New folders are filed under
	// it; folders already backed up keep the name they were filed under.
	MachineName string `json:"machine_name,omitempty"`
	// AccountMode is how this computer keeps its storage keys: "local" on
	// this computer only (moved with an exported key file), "account" synced
	// through the hosted account, or "" before anyone has chosen.
	AccountMode string `json:"account_mode,omitempty"`
	// Purpose is what this computer is using r2backup for, chosen while
	// getting started: "both" (Drime with a second copy on S3-compatible
	// storage) or "s3" (S3 storage only). "" before anyone has chosen.
	Purpose string `json:"purpose,omitempty"`

	// MirrorSources are Drime folders copied to S3 storage for someone who
	// backs up to Drime with another app. See internal/cli/mirror_sources.go.
	MirrorSources []MirrorSource `json:"mirror_sources,omitempty"`

	// MachineNamesBefore are the names this computer went by before, so
	// backups filed under an earlier name still read as this computer's.
	MachineNamesBefore []string `json:"machine_names_before,omitempty"`
}

// MirrorSource is one Drime folder mirrored to S3 storage.
type MirrorSource struct {
	// Name is what it is called here, and the folder it lands in on the
	// destination: drime-mirror/<Name>/.
	Name string `json:"name"`
	// Folder is the path of the folder in Drime, "Backups/Laptop" say.
	Folder string `json:"folder"`
	// Workspace picks a shared Drime workspace; empty is the personal drive.
	Workspace string `json:"workspace,omitempty"`
	// Auto copies it after each scheduled run.
	Auto bool `json:"auto,omitempty"`
	// LastRun is when it was last mirrored in full, Unix seconds.
	LastRun int64 `json:"last_run,omitempty"`
}

// SettingsPath is where the settings file lives.
func SettingsPath() (string, error) { return sub(settingsFileName) }

// LoadSettings reads the settings for this machine.
//
// A missing file is not an error; it is what every fresh install looks like,
// and it reads back as the zero Settings. A corrupt file is an error, so a
// damaged file is never quietly saved over with defaults.
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
