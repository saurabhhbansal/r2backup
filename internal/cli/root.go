// Package cli defines the command surface.
//
// Two rules shape every command here. Nothing prompts during a run -- the
// picker is where choices are made, and after that the tool finishes or reports
// why it could not. And nothing is skipped in silence: a file that could not be
// read is counted, named, and surfaced in status.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/winconsole"
)

// Version is set at build time with -ldflags "-X ...cli.Version=v1.0.0".
var Version = "dev"

// Options carries what every command needs.
type Options struct {
	Out io.Writer
	Err io.Writer
	// In is where a prompt reads its answer. nil means os.Stdin; tests set
	// it so a conversation can be driven without a terminal.
	In io.Reader
	// Yes and No pre-answer the few prompts that exist, for scripts and for
	// scheduled runs. They are mutually exclusive; No is the safe one and wins
	// if both are somehow set.
	Yes bool
	No  bool
}

// Answer is how a command resolves a decision that would otherwise be asked.
type Answer int

const (
	// Ask means a person is present and should be asked.
	Ask Answer = iota
	// AlwaysYes came from --yes.
	AlwaysYes
	// AlwaysNo came from --no, or from running unattended. A scheduled run
	// never prompts: nobody is watching a hidden task at 3am, so the optional
	// work is skipped and the set is parked for a person to look at.
	AlwaysNo
)

// Decision reports how prompts should be handled in this invocation.
func (o *Options) Decision() Answer {
	switch {
	case o.No:
		return AlwaysNo
	case o.Yes:
		return AlwaysYes
	default:
		return Ask
	}
}

// NewRoot builds the command tree.
func NewRoot(opts *Options) *cobra.Command {
	root := &cobra.Command{
		// "r2b", not "r2backup". The product keeps its name; the thing you
		// type does not have to carry it. This is a tool for people who are
		// not at a terminal all day, and every extra character is one they
		// get wrong at 11pm.
		Use:   "r2b",
		Short: "Back up folders to Cloudflare R2 and restore them anywhere",
		Long: "r2b mirrors folders to Cloudflare R2 and restores them anywhere.\n\n" +
			"It does one job. There is no sync, no conflict resolution and no\n" +
			"background service: the OS scheduler runs it and it exits.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// cobra generates a `completion` command on every root command unless
	// told not to. It emits a shell completion script to stdout -- useful to
	// someone who knows what to do with one, and to everyone else it is a
	// command in --help that appears to do nothing and prints several hundred
	// lines of shell if they try it. This help has eleven entries and each one
	// has to earn its place.
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().BoolVar(&opts.Yes, "yes", false,
		"answer yes to any decision, instead of asking")
	root.PersistentFlags().BoolVar(&opts.No, "no", false,
		"answer no to any decision, taking the safe path")
	// Acted on in main, before cobra runs -- see winconsole. It is registered
	// here so cobra accepts it on the scheduled command line rather than
	// failing with "unknown flag", and so it is a real flag with real help
	// text instead of an argument the program secretly understands. Hidden
	// from --help because `schedule` is the only thing that should pass it.
	root.PersistentFlags().Bool(winconsole.HiddenFlagName, false,
		"hide this process's own console window (Windows scheduled runs)")
	_ = root.PersistentFlags().MarkHidden(winconsole.HiddenFlagName)

	root.AddCommand(
		newSetupCmd(opts),
		newAddCmd(opts),
		newEditCmd(opts),
		newBackupCmd(opts),
		newRestoreCmd(opts),
		newStatusCmd(opts),
		newListCmd(opts),
		newScheduleCmd(opts),
		newRenameCmd(opts),
		newRelinkCmd(opts),
		newRemoveCmd(opts),
		newTrashCmd(opts),
		newUpdateCmd(opts),
		newAccountCmd(opts),
	)
	return root
}

// notYet is a placeholder for commands whose implementation lands in a later
// milestone. It fails loudly rather than pretending to succeed, because a
// backup tool that silently does nothing is worse than one that errors.
func notYet(name string) error {
	return fmt.Errorf("%s is not implemented yet", name)
}
