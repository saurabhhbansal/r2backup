// Package schedule registers, inspects and removes the OS scheduler entry
// that runs r2backup every 30 minutes.
//
// r2backup has no daemon and no service. The previous product shipped a
// Windows service and a GUI, and both held their own executable open, so
// Windows Restart Manager could never close them to install an update --
// Setup fell back to demanding a reboot. The fix is structural: nothing of
// ours stays resident between runs. The operating system's own scheduler
// (Task Scheduler, systemd, launchd, or cron) starts the binary, the binary
// does its work and exits, and there is nothing left running for an
// installer to fight with.
//
// This package is the only thing that talks to that scheduler. Each
// platform's implementation lives in its own build-tagged file
// (schedule_windows.go, schedule_linux.go, schedule_darwin.go,
// schedule_unsupported.go for everything else), but the artifact each one
// generates -- the Task Scheduler XML, the systemd unit files, the launchd
// plist, the crontab line -- is produced by a plain function of an Entry
// with no build tag at all, so every format can be generated and checked on
// any platform regardless of which OS is actually running the test.
package schedule

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Entry describes one scheduled run of the r2backup binary: which OS
// scheduler entry to create, how often it should fire, and exactly what to
// execute. r2backup has no daemon, so this is the only place "every 30
// minutes" is expressed anywhere in the system -- the OS scheduler owns it.
type Entry struct {
	// Name identifies the entry to the OS scheduler: the Task Scheduler task
	// name, the systemd unit basename, the launchd Label suffix, and the
	// crontab marker all derive from it. It must stay the same across calls
	// for Install to be idempotent and for Remove to find what Install made.
	Name string
	// Interval is how often the entry fires. Every platform here renders it
	// as a fixed repetition, not a specific time of day.
	Interval time.Duration
	// BinaryPath is the absolute path to the r2backup executable to run.
	BinaryPath string
	// Args are passed to BinaryPath verbatim, e.g. []string{"run", "--all"}.
	Args []string
}

// validate checks the fields every platform's generator and installer
// depend on. Called at the start of every generator and every Install, so a
// bad Entry fails the same way -- and in the same place in the log -- on
// every platform.
func (e Entry) validate() error {
	if e.Name == "" {
		return errors.New("schedule: Entry.Name must not be empty")
	}
	if e.BinaryPath == "" {
		return errors.New("schedule: Entry.BinaryPath must not be empty")
	}
	if e.Interval <= 0 {
		return fmt.Errorf("schedule: Entry.Interval must be positive, got %s", e.Interval)
	}
	return nil
}

// Status reports what the OS scheduler currently knows about an Entry.
//
// NextRun, LastRun and LastResult are best-effort: every platform can answer
// Registered and Interval from the files this package itself wrote, but the
// other three come from parsing a live system's own command output, which
// varies by OS version and locale. A value that can't be determined is left
// at its zero value rather than guessed at.
type Status struct {
	Registered bool
	Interval   time.Duration
	NextRun    time.Time
	LastRun    time.Time
	LastResult string

	// RunsWhenSignedOut reports whether the registered task runs regardless
	// of whether the user is signed in. False both when it genuinely only
	// runs while signed in and when this platform cannot say -- callers use
	// it to soften a claim, never to make one.
	RunsWhenSignedOut bool
}

// ErrUnsupported is wrapped into the error every function in this package
// returns on a platform with no implementation.
var ErrUnsupported = errors.New("schedule: platform not supported")

// unsupportedError is shared by schedule_unsupported.go's Install, Remove
// and Current so the three fail identically instead of each hand-rolling a
// slightly different message.
func unsupportedError(op, name string) error {
	return fmt.Errorf("schedule: %s %q: %w (%s)", op, name, ErrUnsupported, runtime.GOOS)
}

// Runner executes one system command and returns its combined output. Real
// use is defaultRunner; tests substitute a recording fake so Install, Remove
// and Current can be asserted against the exact commands they would have
// run, without a real Task Scheduler, systemd, launchd or cron anywhere
// nearby.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// cmdError wraps a failed system command with what the command actually said.
//
// schtasks, systemctl and launchctl all explain themselves on stdout or
// stderr, and run already captures both -- every call site simply threw the
// text away. What a user got when Task Scheduler refused to register their
// backup was
//
//	schedule: schtasks /create r2backup: exit status 1
//
// which says only that something went wrong, not what, and there is no second
// place to go and look. The reason is the whole value of the error.
func cmdError(op string, out []byte, err error) error {
	if msg := tidyCommandOutput(out); msg != "" {
		return fmt.Errorf("%s: %w: %s", op, err, msg)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// tidyCommandOutput folds a command's output into one line short enough to
// belong in an error. Blank lines go, the rest are joined, and a long tail is
// cut -- schtasks in particular likes to print usage after its message.
func tidyCommandOutput(out []byte) string {
	var kept []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
		if len(kept) == 4 {
			break
		}
	}
	msg := strings.Join(kept, "; ")
	if len(msg) > 400 {
		msg = msg[:400] + "..."
	}
	return msg
}

// run is package-level so every platform file shares one seam. Tests
// override it directly (save the old value, defer restoring it) since they
// live in the same package.
var run Runner = defaultRunner

// lookPath resolves a binary on PATH. It exists as a seam, distinct from
// run, because detecting whether systemd is present on Linux (schedule_linux.go)
// has to happen before there is any command worth running -- Install must
// decide systemd vs. cron before it can build either artifact.
var lookPath = exec.LookPath

// Supported reports whether this package can register scheduled runs on the
// current platform. Implemented per platform file; the fallback in
// schedule_unsupported.go returns false.
// Supported() bool -- declared per platform file, not here: see
// schedule_windows.go, schedule_linux.go, schedule_darwin.go and
// schedule_unsupported.go.

// sanitizeUnitName turns an Entry.Name into something safe to use as a
// systemd unit basename, a launchd Label suffix, or a filename component on
// every platform. Entry.Name is free text chosen by whatever calls this
// package; none of those targets tolerate arbitrary characters (systemd unit
// names reject most punctuation, launchd labels are conventionally
// reverse-DNS-safe, and filenames can't contain a path separator).
func sanitizeUnitName(name string) string {
	if name == "" {
		return "r2backup"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := b.String()
	if s == "" {
		return "r2backup"
	}
	return s
}

// xmlEscapeText escapes s for use as XML element text content. Shared by the
// Windows Task Scheduler XML and the macOS launchd plist, both of which are
// built by hand-written string templates (not encoding/xml.Marshal) so that
// explanatory <!-- comments --> can sit next to the exact setting they
// document -- Marshal has no way to interleave those.
func xmlEscapeText(s string) string {
	var buf bytes.Buffer
	// xml.EscapeText only errors if the writer errors; bytes.Buffer never does.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
