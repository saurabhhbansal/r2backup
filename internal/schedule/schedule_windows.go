//go:build windows

package schedule

import (
	"context"
	"fmt"
	"os"
	"os/user"
)

// Supported reports whether this package can register scheduled runs. Task
// Scheduler is always present on Windows.
func Supported() bool { return true }

// Install registers e as a Task Scheduler task. Calling Install twice with
// the same Name overwrites the existing task in place -- schtasks /create
// /f does that for us, so there is no separate query-then-delete step, and
// an interval change takes effect on the next call with no special casing.
func Install(e Entry) error {
	if err := e.validate(); err != nil {
		return err
	}
	userID := ""
	if u, err := user.Current(); err == nil {
		userID = u.Username
	}
	// S4U first, because a backup that only runs while you happen to be
	// signed in is a worse backup. It is also the one Windows can refuse:
	// registering an S4U task needs the "Log on as a batch job" right, which
	// an ordinary account often does not have, and schtasks then fails --
	// which is exactly what happened on a real desktop, with the reason
	// thrown away. InteractiveToken needs no privilege at all, so it is
	// tried second rather than leaving the user with no schedule.
	ctx := context.Background()
	firstErr := installAs(ctx, e, userID, logonS4U)
	if firstErr == nil {
		return nil
	}
	if err := installAs(ctx, e, userID, logonInteractiveToken); err != nil {
		return firstErr // the S4U failure is the more informative one
	}
	return nil
}

// installAs registers e with one specific logon type. See Install.
func installAs(ctx context.Context, e Entry, userID, logonType string) error {
	content, err := windowsTaskXMLAs(e, userID, logonType)
	if err != nil {
		return fmt.Errorf("schedule: build task XML: %w", err)
	}
	tmp, err := os.CreateTemp("", "r2backup-task-*.xml")
	if err != nil {
		return fmt.Errorf("schedule: create temp task XML file: %w", err)
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)
	// UTF-16 with a BOM: schtasks /create /xml rejects a UTF-8 file outright.
	if err := os.WriteFile(path, utf16BOMBytes(content), 0o600); err != nil {
		return fmt.Errorf("schedule: write task XML: %w", err)
	}
	if out, err := run(ctx, "schtasks", "/create", "/tn", e.Name, "/xml", path, "/f"); err != nil {
		return cmdError(fmt.Sprintf("schedule: schtasks /create %s (%s)", e.Name, logonType), out, err)
	}
	return nil
}

// Remove deletes the Task Scheduler task named name. Not being registered
// at all is not an error.
func Remove(name string) error {
	ctx := context.Background()
	out, err := run(ctx, "schtasks", "/delete", "/tn", name, "/f")
	if err != nil {
		if isTaskNotFound(string(out)) {
			return nil
		}
		return cmdError(fmt.Sprintf("schedule: schtasks /delete %s", name), out, err)
	}
	return nil
}

// Current reports what Task Scheduler currently knows about name.
func Current(name string) (Status, error) {
	ctx := context.Background()
	out, err := run(ctx, "schtasks", "/query", "/tn", name, "/fo", "LIST", "/v")
	if err != nil {
		if isTaskNotFound(string(out)) {
			return Status{}, nil
		}
		return Status{}, cmdError(fmt.Sprintf("schedule: schtasks /query %s", name), out, err)
	}
	st := parseSchtasksListOutput(string(out))
	// The Interval isn't in the /fo LIST view at all; only the /xml export
	// has it, so it takes a second call. Best-effort: an unparsable or
	// missing XML leaves Interval at zero rather than failing Current.
	if xmlOut, err := run(ctx, "schtasks", "/query", "/tn", name, "/xml", "ONE"); err == nil {
		st.RunsWhenSignedOut = windowsParseTaskLogonType(string(xmlOut)) == logonS4U
		if d, perr := windowsParseTaskInterval(string(xmlOut)); perr == nil {
			st.Interval = d
		}
	}
	return st, nil
}
