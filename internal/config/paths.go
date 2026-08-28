// Package config resolves where r2backup keeps its files on each platform and
// reads the small amount of settings that are not per-set.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// EnvDataDir overrides the data directory. Tests set it so they never touch a
// real installation, and it lets a user keep state on another volume.
const EnvDataDir = "R2BACKUP_DATA_DIR"

const appName = "r2backup"

// DataDir is where the index, logs, run history and progress file live.
//
// These follow each platform's own convention rather than one cross-platform
// invention, because a user looking for the files should find them where
// everything else on their system already is.
//
//	Windows  %LOCALAPPDATA%\r2backup
//	macOS    ~/Library/Application Support/r2backup
//	Linux    $XDG_DATA_HOME/r2backup, else ~/.local/share/r2backup
func DataDir() (string, error) {
	if override := os.Getenv(EnvDataDir); override != "" {
		return override, nil
	}
	switch runtime.GOOS {
	case "windows":
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, appName), nil
		}
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", appName), nil
	default:
		if base := os.Getenv("XDG_DATA_HOME"); base != "" {
			return filepath.Join(base, appName), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		return filepath.Join(home, ".local", "share", appName), nil
	}
	// Windows without LOCALAPPDATA, which should not happen but must not panic.
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, appName), nil
}

// EnsureDataDir returns DataDir, creating it if needed.
func EnsureDataDir() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	// 0700: the index records every path in every backed-up folder, which is
	// not something other users on the machine need to read.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create data directory %q: %w", dir, err)
	}
	return dir, nil
}

// IndexPath is the bbolt database recording what has been uploaded.
func IndexPath() (string, error) { return sub("index.db") }

// ProgressPath is rewritten every second by a running backup via atomic rename,
// and read by `r2backup status --watch`.
//
// A file rather than a socket on purpose: a scheduled run and a watching
// terminal are separate processes with no connection between them, and a file
// cannot disagree with itself the way an IPC layer can.
func ProgressPath() (string, error) { return sub("progress.json") }

// LockPath is the per-set lockfile that stops two runs of the same set
// overlapping. It holds a PID so a lock left by a killed run can be told apart
// from one held by a process that is still alive.
func LockPath(set string) (string, error) { return sub("locks", sanitize(set)+".lock") }

// LogPath is the run log for a set.
func LogPath(set string) (string, error) { return sub("logs", sanitize(set)+".log") }

func sub(parts ...string) (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{dir}, parts...)...), nil
}

// sanitize turns a set name into something safe to use as a filename on every
// platform. Set names come from folder names, which can contain separators,
// colons and characters Windows refuses outright.
func sanitize(name string) string {
	if name == "" {
		return "_"
	}
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			out = append(out, '_')
		case r == '/', r == '\\', r == ':', r == '*', r == '?', r == '"',
			r == '<', r == '>', r == '|':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	s := string(out)
	// Windows refuses a filename ending in a dot or a space.
	for len(s) > 0 && (s[len(s)-1] == '.' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	if s == "" {
		return "_"
	}
	return s
}
