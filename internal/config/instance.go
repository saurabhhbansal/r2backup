package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A data directory other than the platform's default -- R2BACKUP_DATA_DIR,
// or --data-dir -- is a separate r2backup: its own folders, index and
// credentials. Anything registered with the operating system has to carry
// that, or it quietly works on the default one instead. A scheduled backup
// runs with none of the environment of the shell that registered it, so the
// directory travels as a flag, and the entry gets a name of its own so two
// data directories do not overwrite each other's schedule.

// DataDirFlagName is the flag that points a process at a data directory. It
// is read off the command line in main, before anything asks for DataDir,
// and registered with cobra so it is also accepted there.
const DataDirFlagName = "data-dir"

// ApplyDataDirArg acts on --data-dir in args by setting R2BACKUP_DATA_DIR,
// which everything else already honours. Scanning stops at "--".
func ApplyDataDirArg(args []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return
		}
		if v, ok := strings.CutPrefix(a, "--"+DataDirFlagName+"="); ok {
			_ = os.Setenv(EnvDataDir, v)
			return
		}
		if a == "--"+DataDirFlagName && i+1 < len(args) {
			_ = os.Setenv(EnvDataDir, args[i+1])
			return
		}
	}
}

// CustomDataDir returns the data directory as an absolute path, and whether
// it is one other than the platform's default.
func CustomDataDir() (string, bool) {
	override := os.Getenv(EnvDataDir)
	if override == "" {
		return "", false
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		abs = override
	}
	def, err := defaultDataDir()
	if err != nil {
		return abs, true
	}
	return abs, !samePath(abs, def)
}

// InstanceName is what this data directory registers with the operating
// system under: the scheduled task, the sign-in entry, the taskbar app's
// one-copy lock. It is "r2backup" for the default directory -- the name
// every existing installation already has -- and "r2backup-" plus a short
// hash of the path for any other.
func InstanceName() string {
	dir, custom := CustomDataDir()
	if !custom {
		return appName
	}
	return instanceName(dir)
}

func instanceName(dir string) string {
	key := filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	sum := sha256.Sum256([]byte(key))
	return appName + "-" + hex.EncodeToString(sum[:4])
}

// DataDirArgs are the arguments that point another r2b process at this data
// directory: none for the default one.
func DataDirArgs() []string {
	dir, custom := CustomDataDir()
	if !custom {
		return nil
	}
	return []string{"--" + DataDirFlagName, dir}
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
