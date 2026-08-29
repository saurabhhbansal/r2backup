// Package winconsole keeps a scheduled run off the screen on Windows.
//
// Task Scheduler cannot launch a process without a window. `<Hidden>true</Hidden>`
// in the task XML reads as though it can, and does not: it hides the task from
// Task Scheduler's own list view and has nothing to do with the console. A
// console-subsystem binary -- which r2backup is, because it is a CLI -- gets a
// console allocated by the loader before main runs, and Task Scheduler starting
// it in the interactive session means that console is on the user's desktop for
// as long as the backup takes. A half-hour backup put a console window on the
// desktop for half an hour.
//
// Under S4U the task runs in a session with no desktop and the question never
// arises, which is why this went unnoticed: it only shows up on the
// InteractiveToken fallback, which is what an account without the "Log on as a
// batch job" right actually gets. That is the common case, not the rare one.
//
// So the process hides its own console, as early in main as possible. The
// window exists for the few milliseconds between process creation and that
// call, which is short enough that it is generally never painted.
package winconsole

// HiddenFlagName is the flag the scheduled command line carries, and
// HiddenFlag is how it is spelled as an argument. It is the whole trigger:
// nothing hides a console unless this was passed, so an interactive run is
// untouched no matter what else is true of its console.
//
// Both the cobra registration in cli.NewRoot and the argument scan below are
// built from these, so the flag the scheduler writes and the flag the program
// parses cannot drift apart.
const (
	HiddenFlagName = "hidden"
	HiddenFlag     = "--" + HiddenFlagName
)

// WantsHidden reports whether args ask for a hidden console.
//
// This is read straight off os.Args rather than from the parsed flag, because
// the console has to go before cobra has assembled a command tree -- every
// millisecond between process start and ShowWindow is a millisecond the window
// might be painted in. Root registers the same flag so cobra accepts it; that
// registration is what makes it a documented flag rather than a magic argument,
// and this function is what acts on it.
//
// A bare "--" ends the flags, so anything after it is an operand and not this
// flag, however it is spelled.
func WantsHidden(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == HiddenFlag {
			return true
		}
	}
	return false
}
