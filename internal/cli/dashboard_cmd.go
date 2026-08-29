package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// runDashboard opens the interface and keeps reopening it around the jobs it
// hands back.
//
// Adding a folder, changing what one includes and restoring one each need
// either the folder picker -- a second full-screen program -- or a line of
// typed input. Rather than reimplement any of that inside the interface, it
// closes, the existing command runs on a clean terminal exactly as it always
// has, and the interface opens again on the result. This is the same handoff
// a git UI makes when it hands you your editor, and it is why there is only
// ever one copy of each of those flows.
func runDashboard(ctx context.Context, opts *Options) error {
	if !interactive() {
		return errors.New("r2b with no arguments opens the interface, which needs a terminal.\n" +
			"  For a script or a scheduled run, name a command: r2b backup, r2b status")
	}
	// Nothing in the interface works without credentials, and "your bucket is
	// empty" is a far worse first screen than being sent to setup.
	if err := haveCredentials(); err != nil {
		return err
	}

	b := &dashboard{opts: opts}
	for {
		action, err := ui.Run(ctx, b)
		if err != nil {
			return err
		}
		switch action.Kind {
		case ui.ActionNone:
			return nil
		case ui.ActionAdd:
			if err := handoff(ctx, opts, "add"); err != nil {
				return err
			}
		case ui.ActionEdit:
			if err := handoff(ctx, opts, "edit", action.Set); err != nil {
				return err
			}
		case ui.ActionRestore:
			if err := handoff(ctx, opts, "restore", action.Set); err != nil {
				return err
			}
		}
	}
}

// handoff runs one command as if it had been typed, then waits for the user
// so its output is read rather than wiped by the interface reopening.
func handoff(ctx context.Context, opts *Options, name string, args ...string) error {
	fmt.Fprintln(opts.Out)
	err := runHandoffCommand(ctx, opts, name, args)
	if err != nil {
		fmt.Fprintf(opts.Err, "\n%s: %v\n", name, err)
	}
	fmt.Fprint(opts.Out, "\nPress enter to go back. ")
	var line [1]byte
	_, _ = os.Stdin.Read(line[:])
	return nil
}

// runHandoffCommand builds a fresh root and executes one command on it. A
// fresh one rather than the running root because cobra keeps parsed flag
// values on the command, and reusing it would carry the previous
// invocation's flags into the next.
func runHandoffCommand(ctx context.Context, opts *Options, name string, args []string) error {
	full := append([]string{name}, args...)
	switch name {
	case "add":
		folder, err := askFolder(opts)
		if err != nil || folder == "" {
			return err
		}
		full = []string{"add", folder}
	case "restore":
		into, err := askDestination(opts, args[0])
		if err != nil {
			return err
		}
		full = []string{"restore", args[0]}
		if into != "" {
			full = append(full, "--to", into)
		}
	}
	root := NewRoot(opts)
	root.SetArgs(full)
	root.SetOut(opts.Out)
	root.SetErr(opts.Err)
	return root.ExecuteContext(ctx)
}

func askFolder(opts *Options) (string, error) {
	p := newPrompter(opts)
	fmt.Fprintln(opts.Out, "Which folder should be backed up? (blank to cancel)")
	answer, err := p.ask("Folder")
	if err != nil {
		return "", err
	}
	return cleanPastedPath(answer), nil
}

func askDestination(opts *Options, set string) (string, error) {
	p := newPrompter(opts)
	fmt.Fprintf(opts.Out, "Restore %q where? Leave blank to put it back where it came from.\n", set)
	answer, err := p.ask("Folder")
	if err != nil {
		return "", err
	}
	return cleanPastedPath(answer), nil
}

// haveCredentials reports whether this machine has been set up, with the
// message that names the one command that fixes it.
func haveCredentials() error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	if _, err := a.creds.Load(); err != nil {
		if errors.Is(err, creds.ErrNotFound) {
			return errors.New("this computer is not set up yet. Run: r2b setup")
		}
		return err
	}
	return nil
}

// attachDashboard makes the interface the default when no command is named,
// while leaving `r2b --help` and every subcommand exactly as they were.
func attachDashboard(root *cobra.Command, opts *Options) {
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return runDashboard(cmd.Context(), opts)
	}
}
