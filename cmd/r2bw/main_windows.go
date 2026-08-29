//go:build windows

// Command r2bw runs r2b with no console window.
//
// Task Scheduler cannot start a process without a window, and <Hidden> in the
// task XML does not do it -- that hides the task from Task Scheduler's own
// list. A console-subsystem binary gets its console from the loader before any
// of its code runs, so r2b cannot win this from the inside: it tries
// (see internal/winconsole) and the window still appears, measured on a real
// desktop. The only way to have no console is to not be a console-subsystem
// binary, which is what this is: built with -H=windowsgui, so Windows never
// allocates one, and it starts r2b with CREATE_NO_WINDOW so the child
// does not get one either.
//
// It is deliberately a launcher and not a second copy of the program. A
// duplicate built from the same source would have to be replaced in step by
// `r2b update`, and a missed update would leave the scheduler quietly
// running last month's backup logic. This has no version and no logic to go
// stale: it runs whatever r2b.exe is sitting next to it, so an update
// that only replaces r2b.exe is still completely correct.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/saurabhhbansal/r2backup/internal/config"
)

func main() { os.Exit(run()) }

func run() int {
	exe, err := os.Executable()
	if err != nil {
		return fail("locate this program", err)
	}
	target := filepath.Join(filepath.Dir(exe), "r2b.exe")
	if _, err := os.Stat(target); err != nil {
		return fail(fmt.Sprintf("find r2b.exe beside %s", exe), err)
	}

	cmd := exec.Command(target, os.Args[1:]...)
	// The whole point. Without it the child, being a console program started
	// by a parent that has no console, allocates and shows one of its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	err = cmd.Run()
	if cmd.ProcessState != nil {
		// Propagated so Task Scheduler's "Last Result" means what it says. A
		// launcher that always exited 0 would report a failed backup as a
		// successful run.
		return cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return fail(fmt.Sprintf("start %s", target), err)
	}
	return 0
}

// fail records why nothing ran and returns a non-zero exit code.
//
// There is no console to print to -- that is the entire purpose of this
// program -- so a failure here would otherwise be perfectly silent, and
// "nothing happened" must never look like "it worked". Task Scheduler shows
// the exit code; this says what it meant. A data directory that cannot be
// written to is not worth failing differently for: the exit code still
// carries.
func fail(what string, err error) int {
	msg := fmt.Sprintf("%s r2bw: could not %s: %v\n",
		time.Now().Format(time.RFC3339), what, err)
	if dir, derr := config.DataDir(); derr == nil {
		if f, oerr := os.OpenFile(filepath.Join(dir, "launcher-errors.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); oerr == nil {
			_, _ = f.WriteString(msg)
			_ = f.Close()
		}
	}
	return 1
}
