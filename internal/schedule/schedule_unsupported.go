//go:build !windows && !linux && !darwin

package schedule

// Supported reports whether this package can register scheduled runs on the
// current platform. r2backup only ships Task Scheduler, systemd/cron and
// launchd implementations, so anything else -- BSD, WASM, whatever else Go
// can target -- has none.
func Supported() bool { return false }

// Install always fails on an unsupported platform. It never panics: the
// caller (the CLI's install command) can report ErrUnsupported to the user
// directly instead of the process crashing.
func Install(e Entry) error { return unsupportedError("install", e.Name) }

// Remove always fails on an unsupported platform, for the same reason.
func Remove(name string) error { return unsupportedError("remove", name) }

// Current always fails on an unsupported platform, for the same reason.
func Current(name string) (Status, error) { return Status{}, unsupportedError("query", name) }
