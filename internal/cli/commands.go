package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/scan"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/trash"
	"github.com/saurabhhbansal/r2backup/internal/tui"
	"github.com/saurabhhbansal/r2backup/internal/winconsole"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// interactive reports whether a person is watching. A scheduled task has no
// terminal, and everything that would ask a question or animate a progress bar
// checks this first.
func interactive() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

func newAddCmd(opts *Options) *cobra.Command {
	var (
		interval  int
		retention int
		all       bool
		name      string
	)
	cmd := &cobra.Command{
		Use:   "add <folder>",
		Short: "Pick what to include in a folder, then back it up",
		Long: "Opens the folder as a tree with everything already selected.\n" +
			"Uncheck what you do not want and press enter; press enter straight\n" +
			"away and all of it goes. This is the only place choices are made.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if err := a.connect(cmd.Context()); err != nil {
				return err
			}

			root, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			info, err := os.Stat(root)
			if err != nil {
				return fmt.Errorf("cannot add %q: %w", root, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%q is not a folder", root)
			}
			if name == "" {
				name = filepath.Base(root)
			}
			// Checked here as well as in sets.Add, so a bad name costs the
			// user an error rather than a full scan of the folder first.
			if err := sets.ValidName(name); err != nil {
				return fmt.Errorf("%q: %w", name, err)
			}
			if _, err := a.sets.Get(name); err == nil {
				return fmt.Errorf("a set called %q already exists; pass --name to choose another", name)
			}
			// Overlapping folders are allowed -- each set carries its own
			// retention and schedule, so wanting one is reasonable -- but
			// every file in the overlap is then stored under two prefixes and
			// paid for twice on every run that touches it. Said once, here,
			// rather than discovered on a bill. A note, not a prompt.
			if other, ok := a.sets.Overlapping(root); ok {
				fmt.Fprintf(opts.Out,
					"Note: this folder overlaps the set %q (%s).\n"+
						"      Files in both are stored twice and cost operations twice.\n",
					other.Name, other.Root)
			}

			fmt.Fprintf(opts.Out, "Scanning %s...\n", root)
			scanned, err := scan.Walk(cmd.Context(), scan.Options{Root: root})
			if err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "%s files · %s\n",
				progress.FormatCount(scanned.Files), progress.FormatBytes(scanned.Bytes))

			var excludes []string
			if !all && interactive() {
				ex, accepted, err := tui.Pick(root, scanned, nil)
				if err != nil {
					return err
				}
				if !accepted {
					fmt.Fprintln(opts.Out, "Cancelled. Nothing was added.")
					return nil
				}
				excludes = ex
			}

			s := sets.Set{
				Name: name, Root: root, Machine: machineName(),
				Prefix:        "machines/" + machineName() + "/" + name,
				Excludes:      excludes,
				RetentionDays: retention,
			}
			// The flag's own default is DefaultRetentionDays, so retention can
			// only be <= 0 here because the user asked for it. Say that in
			// the one way sets.Add will not mistake for "unset".
			if retention <= 0 {
				s.RetentionDays = sets.RetentionDisabled
			}
			if err := a.sets.Add(s); err != nil {
				return err
			}
			stored, err := a.sets.Get(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Added %q. Backing up now.\n\n", name)
			rep, err := runOne(cmd.Context(), a, stored, opts.Out, interactive())
			if err != nil {
				return err
			}
			summarise(opts.Out, rep)
			offerSchedule(opts, interval)
			return nil
		},
	}
	cmd.Flags().IntVar(&interval, "every", schedule.DefaultIntervalMinutes,
		"minutes between automatic backups, if you accept the offer to set them up")
	cmd.Flags().IntVar(&retention, "retention", sets.DefaultRetentionDays,
		"days to keep deleted and overwritten files; 0 disables trash")
	cmd.Flags().BoolVar(&all, "all", false, "skip the picker and include everything")
	cmd.Flags().StringVar(&name, "name", "", "name for this set (default: the folder's name)")
	return cmd
}

func newBackupCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup [set]",
		Short: "Back up now, all sets or one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if err := a.connect(cmd.Context()); err != nil {
				return err
			}
			var only string
			if len(args) == 1 {
				only = args[0]
			}
			list, err := a.resolveSets(only)
			if err != nil {
				return err
			}
			var failed int
			for _, s := range list {
				rep, err := runOne(cmd.Context(), a, s, opts.Out, interactive())
				// A vanished folder is never a deletion. Before anything is
				// reported as a failure, ask where it went -- the person
				// running this is the one who moved it, and telling them to
				// go and run a second command is how a backup quietly stays
				// broken.
				if errors.Is(err, backup.ErrRootMissing) {
					fmt.Fprintf(opts.Out, "%s: %s is not there any more.\n", s.Name, s.Root)
					fmt.Fprintln(opts.Out, "  Nothing has been deleted from your backup.")
					if relinked, ok := offerRelink(opts, a, s); ok {
						// Retried once, and only once: if the folder is not
						// at the new path either, that is the answer.
						rep, err = runOne(cmd.Context(), a, relinked, opts.Out, interactive())
					}
				}
				if err != nil {
					failed++
					fmt.Fprintf(opts.Err, "%s: %v\n", s.Name, err)
					if errors.Is(err, backup.ErrRootMissing) {
						// Parked, so a scheduled run does not keep hitting
						// this, and so status can show it needs a person.
						_ = a.sets.MarkNeedsAttention(s.Name, err.Error())
						fmt.Fprintf(opts.Err,
							"  Nothing was deleted. When you know where it is: r2b relink %q <new-path>\n", s.Name)
					}
					continue
				}
				summarise(opts.Out, rep)
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d set(s) did not finish", failed, len(list))
			}
			return nil
		},
	}
	return cmd
}

// cleanPastedPath makes sense of a path a person typed or pasted at a prompt.
//
// It exists because of how the answer actually arrives. Windows' Explorer
// "Copy as path" wraps the result in double quotes, so a user who does the
// obvious thing pastes `"D:\Photos\2026"`, and a path with those quotes still
// on it does not exist. Trailing whitespace comes free with a paste, and a
// trailing separator is how plenty of people write a folder. None of that is
// user error, and none of it should be answered with "no such directory".
func cleanPastedPath(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{`"`, `'`} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	// A bare "/" or `C:\` is a real root and must keep its separator; only a
	// trailing one on a longer path is noise.
	if len(s) > 1 {
		trimmed := strings.TrimRight(s, `/\`)
		if trimmed != "" && !strings.HasSuffix(trimmed, ":") {
			s = trimmed
		}
	}
	return s
}

// offerRelink asks where a vanished folder went and repoints the set at it,
// returning the updated set and whether the run should be retried.
//
// The alternative it replaces was printing `r2b relink "Photos"
// <new-path>` and giving up. That is a fine answer for someone who lives in a
// terminal and a dead end for everyone else: this tool is meant to be set up
// once and forgotten, and "your backup stopped, here is a command to go and
// look up" is the opposite of that. Nothing is being decided that the user
// does not already know -- they are the one who moved the folder.
//
// It asks only when a person is actually there. A scheduled run has no
// terminal, and a hidden 3am task that blocks on a question is worse than one
// that parks the set and waits: it would sit there until the next reboot,
// backing nothing up and saying nothing. --yes and --no both mean nobody is
// watching, and neither can supply a path anyway, so both skip straight to
// parking. That is the same rule the rest of this package follows, not a new
// one.
func offerRelink(opts *Options, a *app, s sets.Set) (sets.Set, bool) {
	if !interactive() || opts.Decision() != Ask {
		return s, false
	}
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	if !askForNewRoot(opts.Out, in, func(path string) error { return a.sets.Relink(s.Name, path) }) {
		return s, false
	}
	updated, err := a.sets.Get(s.Name)
	if err != nil {
		return s, false
	}
	return updated, true
}

// askForNewRoot runs the prompt loop and reports whether relink succeeded.
//
// Split out from offerRelink so the conversation can be tested without a
// terminal, a set store or a server: everything that decides what the user
// sees is here, and everything that needs the world is behind `relink`. The
// first attempt to test this end to end hung, because the loop read os.Stdin
// directly and a pty with nothing left to give never returns.
func askForNewRoot(out io.Writer, in io.Reader, relink func(string) error) bool {
	fmt.Fprintf(out, "\nDid you move or rename it? Type or paste where it is now.\n")
	r := bufio.NewReader(in)
	// A few tries, because a mistyped path is the expected mistake here and
	// answering it with a dead end is what this whole prompt exists to avoid.
	// Not unlimited: input that keeps failing must not spin.
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprint(out, "New location (or press Enter to leave it for now): ")
		line, err := r.ReadString('\n')
		path := cleanPastedPath(line)
		if path == "" {
			// Enter, EOF and a closed stdin all mean the same thing: no
			// answer is coming, and nothing should be guessed.
			fmt.Fprintln(out)
			return false
		}
		if rerr := relink(path); rerr != nil {
			fmt.Fprintf(out, "  %v\n", rerr)
			if err != nil {
				return false // nothing more to read; do not re-prompt into EOF
			}
			continue
		}
		fmt.Fprintf(out, "Relinked. Nothing has to be uploaded again.\n\n")
		return true
	}
	return false
}

func newStatusCmd(opts *Options) *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "What ran, when, and what is next",
		RunE: func(cmd *cobra.Command, args []string) error {
			// --watch reads only progress.json, the same file a run in any
			// other process writes -- see watchProgress. It never touches
			// sets.json, credentials or the index, so it must not open any
			// of them either: opening the index here is exactly the call
			// that used to fail this command while a backup or the
			// dashboard held the lock, on the one variant of `status` whose
			// entire purpose is watching a run that is, by definition,
			// already holding it.
			if watch {
				return watchProgress(cmd.Context(), opts)
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			return printStatus(a, opts)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false,
		"follow a run already in progress, including one started by the scheduler")
	return cmd
}

func printStatus(a *app, opts *Options) error {
	list := a.sets.List()
	if len(list) == 0 {
		fmt.Fprintln(opts.Out, "Nothing is being backed up yet. Run: r2b add <folder>")
		return nil
	}
	histPath, _ := historyPath()
	hist, _ := runstate.ReadHistory(histPath)

	idx, release, err := a.checkoutIndex()
	if err != nil {
		return err
	}
	defer release()

	progressPath, _ := config.ProgressPath()
	now := time.Now()
	if live, err := runstate.ReadLive(progressPath); err == nil && !live.Stale(now) {
		fmt.Fprintf(opts.Out, "Running now: %s — %s of %s, %s\n\n", live.Set,
			progress.FormatBytes(live.BytesDone), progress.FormatBytes(live.BytesTotal),
			etaText(live))
	} else if stopped, ok := runstate.ReadInterrupted(progressPath, now); ok {
		// A run that was stopped rather than finished. It said nothing at
		// all before: the file was there, every reader saw it was stale, and
		// every reader threw it away.
		fmt.Fprintf(opts.Out, "Interrupted: %s stopped %s — %s of %s\n",
			stopped.Set, humanAgo(now.Sub(stopped.UpdatedAt)),
			progress.FormatBytes(stopped.BytesDone), progress.FormatBytes(stopped.BytesTotal))
		if done, total, files, err := idx.PendingBytes(); err == nil && files > 0 {
			fmt.Fprintf(opts.Out, "  %s of %s already uploaded across %s, and kept.\n",
				progress.FormatBytes(done), progress.FormatBytes(total),
				countOf(int64(files), "part-uploaded file", "part-uploaded files"))
		}
		fmt.Fprintln(opts.Out, "  It carries on from there by itself: at the next scheduled run,")
		fmt.Fprintln(opts.Out, "  or when you next sign in.")
		fmt.Fprintln(opts.Out)
	}

	for _, s := range list {
		fmt.Fprintf(opts.Out, "%s\n  %s\n", s.Name, s.Root)
		if s.Status == sets.StatusNeedsAttention {
			fmt.Fprintf(opts.Out, "  NEEDS ATTENTION: %s\n", s.StatusNote)
		}
		if last, ok := hist.Last(s.Name); ok {
			when := humanAgo(time.Since(last.FinishedAt))
			if last.Cancelled {
				// Not "failed": last.Error here is backup.ErrCancelled's
				// text, and a run someone stopped on purpose is not a
				// failure -- printing "cancelled" says what happened
				// instead of making a deliberate stop sound like a bug.
				fmt.Fprintf(opts.Out, "  last run %s — cancelled\n", when)
			} else if last.Error != "" {
				fmt.Fprintf(opts.Out, "  last run %s — failed: %s\n", when, last.Error)
			} else {
				fmt.Fprintf(opts.Out, "  last run %s — %s uploaded, %s unchanged, %s operations\n",
					when, progress.FormatCount(int64(last.Uploaded)),
					progress.FormatCount(int64(last.Unchanged)),
					progress.FormatCount(int64(last.Operations)))
				if last.Failures > 0 || last.Collisions > 0 || last.Problems > 0 {
					fmt.Fprintf(opts.Out, "  %d failed, %d unreadable, %d name collisions\n",
						last.Failures, last.Problems, last.Collisions)
				}
			}
		} else {
			fmt.Fprintln(opts.Out, "  never run")
		}
		fmt.Fprintln(opts.Out)
	}

	// The operation count against the free tier, counted locally because we
	// perform every one of them ourselves.
	if used, resetAt, err := idx.OpsThisMonth(); err == nil {
		fmt.Fprintf(opts.Out, "Operations this month: %s of %s free (resets %s)\n",
			progress.FormatCount(int64(used)),
			progress.FormatCount(int64(index.FreeTierOpsPerMonth)),
			resetAt.Format("2 Jan"))
	}
	if st, err := schedule.Current("r2backup"); err == nil && st.Registered {
		fmt.Fprintf(opts.Out, "Scheduled: every %s\n", st.Interval)
	} else {
		fmt.Fprintln(opts.Out, "Not scheduled. To run automatically: r2b schedule --every 30")
	}
	return nil
}

func etaText(l *runstate.Live) string {
	if !l.ETAKnown {
		return "estimating..."
	}
	return progress.FormatDuration(time.Duration(l.ETASeconds*float64(time.Second))) + " remaining"
}

func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// watchProgress follows a run started elsewhere -- typically by the scheduler.
// It reads the same file the running process writes; there is no socket and no
// server, so the two cannot disagree about what is happening.
func watchProgress(ctx context.Context, opts *Options) error {
	path, err := config.ProgressPath()
	if err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var printed bool
	for {
		live, err := runstate.ReadLive(path)
		switch {
		case err == nil && !live.Stale(time.Now()):
			if printed {
				fmt.Fprint(opts.Out, "\033[1A\033[2K")
			}
			fmt.Fprintf(opts.Out, "%s: %s/%s · %s files · %s\n", live.Set,
				progress.FormatBytes(live.BytesDone), progress.FormatBytes(live.BytesTotal),
				progress.FormatCount(live.FilesDone), etaText(live))
			printed = true
		case printed:
			fmt.Fprintln(opts.Out, "Run finished.")
			return nil
		default:
			fmt.Fprintln(opts.Out, "No backup is running.")
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func newListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "ls [set]",
		Short: "What is in the backup",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			var only string
			if len(args) == 1 {
				only = args[0]
			}
			list, err := a.resolveSets(only)
			if err != nil {
				return err
			}
			idx, release, err := a.checkoutIndex()
			if err != nil {
				return err
			}
			defer release()
			for _, s := range list {
				n, err := idx.Count(s.Name)
				if err != nil {
					return err
				}
				fmt.Fprintf(opts.Out, "%s: %s\n", s.Name, countOf(int64(n), "object", "objects"))
				if only != "" {
					recs, err := idx.All(s.Name)
					if err != nil {
						return err
					}
					for _, r := range recs {
						fmt.Fprintf(opts.Out, "  %10s  %s\n", progress.FormatBytes(r.Size), r.Key)
					}
				}
			}
			return nil
		},
	}
}

// countOf renders a count with its noun, singular or plural: "1 object",
// "1,204 objects". Grouping comes from progress.FormatCount, so a number the
// user reads here is spelled the same way it is everywhere else.
func countOf(n int64, one, many string) string {
	noun := many
	if n == 1 {
		noun = one
	}
	return progress.FormatCount(n) + " " + noun
}

// installSchedule registers the OS scheduler entry and explains what the user
// actually got.
//
// Shared by `schedule` and by the offer `add` makes, so the two cannot drift:
// the launcher lookup, the fallback, and the two notes Windows can force are
// stated once. They are the difference between a backup that runs when you
// are signed out and one that does not, and between a silent run and one that
// flashes a console window.
func installSchedule(opts *Options, every int) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	binary, windowless := scheduledBinary(runtime.GOOS, self, fileExists)
	if err := schedule.Install(schedule.Entry{
		Name:       "r2backup",
		Interval:   time.Duration(every) * time.Minute,
		BinaryPath: binary,
		Args:       scheduledRunArgs(runtime.GOOS),
	}); err != nil {
		return err
	}
	if windowless {
		fmt.Fprintf(opts.Out, "Registered. Backups run every %d minutes, out of sight, and survive a reboot.\n", every)
	} else {
		// Said plainly rather than left to be discovered. A console window
		// appearing every half hour on a tool sold as invisible is exactly
		// the sort of thing a user assumes is broken.
		fmt.Fprintf(opts.Out, "Registered. Backups run every %d minutes and survive a reboot.\n", every)
		fmt.Fprintf(opts.Out,
			"Note: a console window will appear briefly on each run. %s is missing\n"+
				"      from %s; reinstall r2backup to get it back.\n",
			LauncherName, filepath.Dir(self))
	}
	// On Windows the preferred registration can be refused and a second one
	// used instead, and the difference matters: one runs whether or not you
	// are signed in, the other does not. Read it back rather than claiming
	// whichever was asked for first.
	if st, err := schedule.Current("r2backup"); err == nil && st.Registered && !st.RunsWhenSignedOut && runtime.GOOS == "windows" {
		fmt.Fprintln(opts.Out,
			"Note: it runs while you are signed in. Windows would not grant the\n"+
				"      permission needed to run it when you are signed out, which\n"+
				"      needs the \"Log on as a batch job\" right for your account.")
	}
	fmt.Fprintln(opts.Out, "To watch one: r2b status --watch")
	return nil
}

// askYesNo puts a yes/no question and returns the answer, or def when there
// is nothing to read. Enter takes the default, which is shown capitalised.
func askYesNo(out io.Writer, in io.Reader, question string, def bool) bool {
	choices := "[y/N]"
	if def {
		choices = "[Y/n]"
	}
	fmt.Fprintf(out, "%s %s ", question, choices)
	line, err := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	if err != nil && strings.TrimSpace(line) == "" {
		// EOF with nothing typed: no answer is coming. Take the default
		// rather than hanging or guessing the other way.
		fmt.Fprintln(out)
	}
	return def
}

// offerSchedule asks, once, whether backups should run by themselves.
//
// `add` used to end with "To have this run by itself: r2b schedule
// --every 30" -- a command the user had to notice, remember and run. Anyone
// who did not was left with a folder that is backed up exactly once, which is
// the opposite of what they asked for, and nothing said so again except
// `status`. This is the single place that mattered most for a tool meant to
// be set up and forgotten.
//
// It asks only when a person is there, like every other prompt here: an
// unattended `add` prints the command instead. And it never changes a
// schedule that already exists -- a second `add` should not silently
// re-time the first one.
func offerSchedule(opts *Options, every int) {
	if st, err := schedule.Current("r2backup"); err == nil && st.Registered {
		fmt.Fprintf(opts.Out, "\nBackups already run every %s. This folder is included from now on.\n",
			st.Interval.Round(time.Minute))
		return
	}
	if !interactive() || opts.Decision() != Ask || !schedule.Supported() {
		fmt.Fprintf(opts.Out, "\nTo have this run by itself: r2b schedule --every %d\n", every)
		return
	}
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	fmt.Fprintln(opts.Out)
	if !askYesNo(opts.Out, in,
		fmt.Sprintf("Back up automatically every %d minutes from now on?", every), true) {
		fmt.Fprintf(opts.Out, "Not scheduled. To do it later: r2b schedule --every %d\n", every)
		return
	}
	if err := installSchedule(opts, every); err != nil {
		// Not fatal: the folder is added and backed up, which is what the
		// command was for. Say what did not happen rather than failing the
		// whole thing after the work succeeded.
		fmt.Fprintf(opts.Err, "Could not set up the schedule: %v\n", err)
		fmt.Fprintf(opts.Err, "To try again: r2b schedule --every %d\n", every)
	}
}

// newEditCmd builds `edit`, which reopens the picker on a set that already
// exists.
//
// The picker had exactly one caller -- `add` -- so what a set included was
// decided once, at the moment it was created, and could never be changed
// again. The only way to exclude a node_modules you had not thought about was
// to hand-edit sets.json, which is not a thing to ask of someone who is not a
// programmer. Everything the picker needs was already there; nothing could
// reach it.
//
// Changing the excludes is not a destructive act, and the run afterwards
// makes that true rather than merely claimed: objects that fall out of the
// mirror are moved to trash on the way out, so they stay recoverable for the
// set's retention window exactly like a deleted file.
func newEditCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "edit <set>",
		Short: "Change what is included in a set",
		Long: "Reopens the folder picker with the current selection, then backs up\n" +
			"with whatever you choose. Newly excluded files move to trash, so\n" +
			"they stay recoverable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() {
				return errors.New("edit opens the folder picker, which needs a terminal")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			s, err := a.sets.Get(args[0])
			if err != nil {
				return err
			}
			if err := a.connect(cmd.Context()); err != nil {
				return err
			}

			fmt.Fprintf(opts.Out, "Scanning %s...\n", s.Root)
			scanned, err := scan.Walk(cmd.Context(), scan.Options{Root: s.Root})
			if err != nil {
				// Same answer as a backup gives: ask where it went rather
				// than printing a command to go and run.
				if errors.Is(err, scan.ErrRootMissing) {
					fmt.Fprintf(opts.Out, "%s: %s is not there any more.\n", s.Name, s.Root)
					fmt.Fprintln(opts.Out, "  Nothing has been deleted from your backup.")
					relinked, ok := offerRelink(opts, a, s)
					if !ok {
						return err
					}
					s = relinked
					if scanned, err = scan.Walk(cmd.Context(), scan.Options{Root: s.Root}); err != nil {
						return err
					}
				} else {
					return err
				}
			}

			// Opened showing what is backed up now, not a blank slate: this
			// is an edit, and starting again from "everything" would quietly
			// undo every choice already made.
			chosen, accepted, err := tui.Pick(s.Root, scanned, s.Excludes)
			if err != nil {
				return err
			}
			if !accepted {
				fmt.Fprintln(opts.Out, "Cancelled. Nothing was changed.")
				return nil
			}

			added, removed := diffExcludes(s.Excludes, chosen)
			if len(added) == 0 && len(removed) == 0 {
				fmt.Fprintln(opts.Out, "No change.")
				return nil
			}
			s.Excludes = chosen
			if err := a.sets.Update(s); err != nil {
				return err
			}
			for _, e := range added {
				fmt.Fprintf(opts.Out, "  now excluded: %s\n", e)
			}
			for _, e := range removed {
				fmt.Fprintf(opts.Out, "  now included: %s\n", e)
			}
			if len(added) > 0 {
				fmt.Fprintln(opts.Out, "Excluded files move to trash and stay recoverable.")
			}

			fmt.Fprintln(opts.Out)
			rep, err := runOne(cmd.Context(), a, s, opts.Out, interactive())
			if err != nil {
				return err
			}
			summarise(opts.Out, rep)
			return nil
		},
	}
}

// diffExcludes reports what the user just excluded and what they just let
// back in, so the change is stated in their terms rather than as two lists to
// compare by eye.
func diffExcludes(before, after []string) (added, removed []string) {
	was := make(map[string]bool, len(before))
	for _, e := range before {
		was[e] = true
	}
	now := make(map[string]bool, len(after))
	for _, e := range after {
		now[e] = true
	}
	for _, e := range after {
		if !was[e] {
			added = append(added, e)
		}
	}
	for _, e := range before {
		if !now[e] {
			removed = append(removed, e)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// newRemoveCmd builds `remove`.
//
// Nothing could stop backing up a folder: sets.Store.Remove and
// index.DropSet both existed and no command called either, so adding the
// wrong folder was permanent. The question that kept it unbuilt is what
// happens to the objects, and the answer is that the user says which.
//
// The default keeps them. Deleting someone's backup because they stopped
// tracking a folder is never the safe reading of the request, and this is the
// most destructive command in the product -- so the destructive half needs its
// own word on the command line. Typing --purge *is* the confirmation: "no
// second prompts" is a design decision here, and a y/N that everyone learns to
// answer "y" to is not a safety feature.
//
// --purge deletes permanently rather than routing through trash, which looks
// like the kinder option and is not. Trash is reachable only through a live
// set: `trash ls` and `restore --deleted` both resolve a set by name. Trashing
// the objects and then removing the set would leave them unreachable by any
// command and still billed every month -- recoverable in name only. The
// recoverable option is the default, which keeps the live mirror intact and
// restorable.
func newRemoveCmd(opts *Options) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "remove <set>",
		Short: "Stop backing up a folder",
		Long: "Forgets the folder on this computer. The backup in the bucket is kept,\n" +
			"and adding the folder again reaches it, unless you pass --purge.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			// Resolved before anything is changed, so an unknown name is an
			// error and not a cheerful report of having removed nothing.
			s, err := a.sets.Get(args[0])
			if err != nil {
				return err
			}

			var deleted remote.PrefixDeletion
			if purge {
				if err := a.connect(cmd.Context()); err != nil {
					return err
				}
				// Deleted first, and the set forgotten only if that worked.
				// The other order loses the only handle on these objects the
				// moment the delete fails partway. KeyScope, never Prefix: a
				// bare prefix also matches a set whose name merely starts
				// with this one's.
				var err error
				deleted, err = a.client.DeletePrefix(cmd.Context(), s.KeyScope())
				if err != nil {
					return fmt.Errorf("%q was not removed: %w", s.Name, err)
				}
			}

			// The index is a cache of what is already uploaded, keyed by set
			// name, and it has to go with the set: left behind, a later set
			// of the same name inherits it and skips uploading a file it has
			// never seen, because the record says it is already there.
			//
			// Dropped before the set is forgotten, because the two steps can
			// fail independently and the orders are not equally bad. This way
			// a failure leaves a set whose index is empty, and the next run
			// uploads the tree again -- expensive, never wrong. The other way
			// leaves records with no set, waiting for the next set of that
			// name to inherit them and skip files.
			idx, release, err := a.checkoutIndex()
			if err != nil {
				return err
			}
			defer release()
			if err := idx.DropSet(s.Name); err != nil {
				return err
			}
			// Same reasoning: a later set of this name must not inherit a
			// claim that makes it skip its first trash sweep.
			if err := idx.ForgetDailyPrune(s.Name); err != nil {
				return err
			}
			// And its unfinished uploads: left behind, they are not just an
			// entry the sweep keeps asking the bucket about for a folder
			// nobody backs up any more -- they are parts the server is still
			// billing for, with no completed object in any listing to
			// explain the charge, and once the record below is gone there is
			// no way to ever find them again. So they are aborted on the
			// server first, while the index can still say what they were.
			aborted, failedAbort := a.abortSetUploads(cmd.Context(), idx, s, purge,
				func() error { return a.connect(cmd.Context()) })
			if err := idx.DropSetUploads(s.KeyScope()); err != nil {
				return err
			}
			if err := a.sets.Remove(s.Name); err != nil {
				return err
			}

			switch {
			case purge && deleted.Objects == 0:
				// Nothing was deleted, so saying it cannot be undone would be
				// describing an event that did not happen.
				fmt.Fprintf(opts.Out, "Removed %q. There was nothing in the bucket under %s to delete.\n",
					s.Name, s.KeyScope())
			case purge:
				fmt.Fprintf(opts.Out, "Removed %q and deleted %s (%s) from the bucket. This cannot be undone.\n",
					s.Name, countOf(int64(deleted.Objects), "object", "objects"), progress.FormatBytes(deleted.Bytes))
			default:
				fmt.Fprintf(opts.Out, "Removed %q. This computer no longer backs up %s.\n", s.Name, s.Root)
				fmt.Fprintf(opts.Out, "The backup is still in the bucket under %s and nothing will delete it.\n", s.KeyScope())
				fmt.Fprintf(opts.Out, "To get it back: r2b add %s --name %s\n", s.Root, s.Name)
				// Said plainly because it is a real cost and the whole
				// argument of this tool is the operations budget. The index
				// went with the set, and a run reads the index rather than
				// the bucket to decide what is already there -- so the first
				// backup after re-adding uploads the whole folder again over
				// the top of objects that are already correct.
				fmt.Fprintln(opts.Out, "Re-adding it uploads the folder once more: what is already uploaded is")
				fmt.Fprintln(opts.Out, "tracked on this computer, and that record went with the set.")
			}
			// Said for the same reason r2b backup reports AbandonStaleUploads:
			// these are billed and show up in no object listing, so this is
			// the only place the charge is ever explained.
			switch {
			case aborted == 0 && failedAbort == 0:
				// Nothing pending, nothing to say.
			case failedAbort == 0:
				fmt.Fprintf(opts.Out, "Also stopped %s that had never finished; nothing more will be billed for them.\n",
					countOf(int64(aborted), "unfinished upload", "unfinished uploads"))
			case aborted == 0:
				fmt.Fprintf(opts.Out, "%s that had never finished could not be reached and may still be billed. Check the bucket by hand.\n",
					countOf(int64(failedAbort), "unfinished upload", "unfinished uploads"))
			default:
				fmt.Fprintf(opts.Out, "Also stopped %s that had never finished; %s could not be reached and may still be billed.\n",
					countOf(int64(aborted), "unfinished upload", "unfinished uploads"),
					countOf(int64(failedAbort), "unfinished upload", "unfinished uploads"))
			}
			// A scheduled task that has nothing left to back up fails every
			// time it fires, on a machine where nobody is watching it.
			if len(a.sets.List()) == 0 {
				if st, err := schedule.Current("r2backup"); err == nil && st.Registered {
					fmt.Fprintln(opts.Out, "\nNothing is being backed up now, and a scheduled run is still registered.")
					fmt.Fprintln(opts.Out, "To unregister it: r2b schedule --remove")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false,
		"also delete this set's objects from the bucket, permanently and unrecoverably")
	return cmd
}

// LauncherName is the window-less companion beside r2backup on Windows.
// See cmd/r2bw.
const LauncherName = "r2bw.exe"

// scheduledBinary picks what the OS scheduler should actually run, and
// reports whether that choice puts nothing on screen.
//
// On Windows the answer is the launcher beside this binary when it is there.
// Registering r2b.exe directly means Task Scheduler starts a
// console-subsystem program in the interactive session, and a console window
// appears for the length of the backup -- measured on a real desktop, both
// before and after r2backup started hiding its own console, because the
// loader creates that console before any of its code runs.
//
// It falls back rather than failing. Someone running a build from `go build`,
// or an install where only the one file was copied, still gets a working
// schedule; they are told a window will appear, which is true, instead of
// being told nothing and seeing one anyway.
//
// exists is a parameter so the fallback can be tested from any platform --
// the case that matters is a Windows machine without the launcher, and there
// is no Windows machine in CI that can be missing a file that CI just built.
func scheduledBinary(goos, self string, exists func(string) bool) (path string, windowless bool) {
	if goos != "windows" {
		// systemd and launchd start a process with no terminal at all, so
		// there was never a window to avoid.
		return self, true
	}
	launcher := filepath.Join(filepath.Dir(self), LauncherName)
	if exists(launcher) {
		return launcher, true
	}
	return self, false
}

// fileExists is scheduledBinary's real-world lookup.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// scheduledRunArgs is the command line the OS scheduler is given.
//
// On Windows it carries --hidden. Task Scheduler cannot start a process
// without a window, and <Hidden> in the task XML does not do it -- that hides
// the task from Task Scheduler's own list, not the console. A run in the
// interactive session, which is what an account without the "Log on as a
// batch job" right gets, therefore puts a console window on the desktop for
// as long as the backup takes. The process closes its own; see
// internal/winconsole.
//
// goos is a parameter rather than read from runtime so the Windows command
// line can be asserted from any platform -- the CI leg that would have caught
// the console window is a person looking at a desktop, and there isn't one.
func scheduledRunArgs(goos string) []string {
	args := []string{"backup"}
	if goos == "windows" {
		args = append(args, winconsole.HiddenFlag)
	}
	return args
}

func newScheduleCmd(opts *Options) *cobra.Command {
	var (
		every  int
		remove bool
		repair bool
	)
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Register with the operating system's scheduler",
		Long: "Registers a Task Scheduler entry on Windows, a systemd user timer\n" +
			"on Linux, or a launchd agent on macOS. Runs put nothing on screen.\n" +
			"Nothing of ours stays running in between, so updating the binary is\n" +
			"never blocked.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !schedule.Supported() {
				return errors.New("no scheduler is available on this platform")
			}
			if remove {
				// Ask first, so this does not claim to have removed
				// something that was never there. Remove itself treats "no
				// such task" as success, which is right for the operation
				// and wrong for the sentence printed afterwards.
				before, cerr := schedule.Current("r2backup")
				if err := schedule.Remove("r2backup"); err != nil {
					return err
				}
				switch {
				case cerr != nil:
					// Could not tell either way; do not guess which it was.
					fmt.Fprintln(opts.Out, "No scheduled task remains. Backups will only run when you run them.")
				case before.Registered:
					fmt.Fprintln(opts.Out, "Unregistered. Backups will only run when you run them.")
				default:
					fmt.Fprintln(opts.Out, "There was no scheduled task to remove. Backups only run when you run them.")
				}
				return nil
			}
			if repair {
				// Point an existing entry at wherever the binary lives now,
				// keeping the interval it already has. The installers call
				// this after replacing the files: the OS scheduler stores an
				// absolute path, so renaming the executable -- which the move
				// from r2backup.exe to r2b.exe is -- leaves the task aimed at
				// a file that no longer exists, and a backup that silently
				// stops running is the worst way for this to fail.
				//
				// A machine with no schedule is left with no schedule. An
				// installer must not quietly start backing things up on a
				// timer nobody asked for.
				st, err := schedule.Current("r2backup")
				if err != nil || !st.Registered {
					fmt.Fprintln(opts.Out, "Nothing scheduled; nothing to repair.")
					return nil
				}
				return installSchedule(opts, int(st.Interval.Round(time.Minute)/time.Minute))
			}
			return installSchedule(opts, every)
		},
	}
	cmd.Flags().IntVar(&every, "every", schedule.DefaultIntervalMinutes, "minutes between runs")
	cmd.Flags().BoolVar(&remove, "remove", false, "unregister instead")
	cmd.Flags().BoolVar(&repair, "repair", false,
		"re-point an existing schedule at this copy of the program, keeping its interval")
	return cmd
}

// newRenameCmd builds `rename`, which changes the display name and nothing
// else.
//
// There is deliberately no way to move the set's objects to a prefix matching
// the new name. It was offered once, as `--remote`, and never implemented; the
// flag is gone rather than finished. Moving a prefix is a server-side copy and
// a delete of every object -- two operations each, 122,408 of them for a set
// of 61,204 files -- to change a name only visible in the R2 dashboard. The
// prefix is the set's identity, assigned once and never rewritten, because
// tying identity to something a user can edit is how the predecessor orphaned
// data in a bucket.
//
// Nothing detects a situation where this would be the answer, either. The
// case that looks like it -- a backed-up folder renamed or moved on disk -- is
// `relink`, which points the set at the new path and moves nothing in the
// bucket, because the objects there were never wrong.
func newRenameCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <set> <new-name>",
		Short: "Change what a set is called",
		Long: "Changes the name only. The bucket keeps the prefix it was created\n" +
			"with, so this is instant and costs nothing.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			s, err := a.sets.Get(args[0])
			if err != nil {
				return err
			}
			idx, release, err := a.checkoutIndex()
			if err != nil {
				return err
			}
			defer release()
			// The index is keyed by set name too. Move it first: it is one
			// bbolt transaction and so cannot half-happen, and if the set
			// store then refuses the new name the index goes back where it
			// was. The other order has no recovery -- it is what left a
			// renamed set reading an empty index and re-uploading everything.
			if err := idx.RenameSet(args[0], args[1]); err != nil {
				return err
			}
			if err := a.sets.Rename(args[0], args[1]); err != nil {
				if back := idx.RenameSet(args[1], args[0]); back != nil {
					return fmt.Errorf("%w (and the index could not be put back under %q: %v -- "+
						"run `r2b backup %s` to rebuild it, which re-uploads the set)",
						err, args[0], back, args[1])
				}
				return err
			}
			n, _ := idx.Count(args[1])
			fmt.Fprintf(opts.Out, "Renamed to %q.\n", args[1])
			fmt.Fprintf(opts.Out,
				"The bucket still stores it under %q, and keeps that name for good. Moving\n"+
					"%s objects to match would cost two operations each, to change a name\n"+
					"only the R2 dashboard shows.\n", s.Prefix, progress.FormatCount(int64(n)))
			return nil
		},
	}
	return cmd
}

func newRelinkCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "relink <set> <new-path>",
		Short: "Point a set at a folder that moved",
		Long: "Use this when a backed-up folder was renamed or moved. Nothing is\n" +
			"re-uploaded: the objects in the bucket are still correct, only the\n" +
			"local path changed.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if err := a.sets.Relink(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "%q now points at %s. Nothing was re-uploaded.\n", args[0], args[1])
			return nil
		},
	}
}

func newTrashCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "What is recoverable, and until when",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "ls [set]",
		Short: "List recoverable files and their expiry",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if err := a.connect(cmd.Context()); err != nil {
				return err
			}
			var only string
			if len(args) == 1 {
				only = args[0]
			}
			list, err := a.resolveSets(only)
			if err != nil {
				return err
			}
			tr := trash.New(a.client, trash.Clock{})
			for _, s := range list {
				// Listing a set with no trash would date every entry against
				// a retention window that does not exist. There is nothing to
				// list, and why is more use than "0 recoverable".
				if !s.TrashEnabled() {
					fmt.Fprintf(opts.Out, "%s: trash is off for this set, so nothing is recoverable.\n", s.Name)
					continue
				}
				entries, err := tr.List(cmd.Context(), s.Prefix, s.RetentionDays)
				if err != nil {
					return err
				}
				fmt.Fprintf(opts.Out, "%s: %d recoverable\n", s.Name, len(entries))
				for _, e := range entries {
					fmt.Fprintf(opts.Out, "  %10s  %s  (until %s)\n",
						progress.FormatBytes(e.Size), e.RelPath, e.ExpiresOn.Format("2 Jan"))
				}
			}
			return nil
		},
	})
	return cmd
}

func newRestoreCmd(opts *Options) *cobra.Command {
	var (
		to        string
		only      string
		machine   string
		overwrite bool
		verify    bool
		deleted   string
	)
	cmd := &cobra.Command{
		Use:   "restore <set>",
		Short: "Bring a folder back",
		Long: "Restores to the folder's original path when that path exists on this\n" +
			"machine, and otherwise requires --to. It never guesses and never asks.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if err := a.connect(cmd.Context()); err != nil {
				return err
			}
			// Local record first, bucket second. A computer that has just run
			// setup has no sets.json at all, and telling it "no such set"
			// about data it can see and pay for is the wrong answer -- see
			// resolveRestoreSet.
			s, err := resolveRestoreSet(cmd.Context(), a, args[0], machine)
			if err != nil {
				return err
			}
			rep, err := restore.Run(cmd.Context(), restore.Options{
				Set:           s,
				Client:        a.client,
				Target:        to,
				Only:          only,
				SourceMachine: machine,
				Deleted:       deleted,
				Overwrite:     overwrite,
				Verify:        verify,
				Observer:      &restoreObserver{out: opts.Out, set: s.Name, interactive: interactive()},
			})
			if err != nil {
				if errors.Is(err, restore.ErrNoTarget) {
					if s.Root == "" {
						// Discovered in the bucket: there is no original
						// path to report, because this computer never had one.
						return fmt.Errorf("%w\n  %q was backed up from %s, so there is no folder here to put it back into.\n"+
							"  Say where to put it: r2b restore %s --to <folder>", err, s.Name, s.Machine, s.Name)
					}
					return fmt.Errorf("%w\n  The original folder %q is not on this machine.\n"+
						"  Say where to put it: r2b restore %q --to <folder>", err, s.Root, s.Name)
				}
				return err
			}
			// A restore that found nothing must not read as a restore that
			// worked. `--machine typo-pc` used to print "0 restored" and exit
			// 0, which is the worst possible answer to "is my data there?" --
			// the same reason `--deleted` on an unknown path already fails
			// rather than succeeding quietly. ListedFiles counts what the
			// LIST returned, before --only narrows it, so these two cases
			// stay distinguishable.
			if rep.ListedFiles == 0 {
				if machine != "" {
					return fmt.Errorf("nothing is stored for %q under machine %q, so nothing was restored. "+
						"Check the name with `r2b account devices`", s.Name, machine)
				}
				return fmt.Errorf("nothing is stored for %q yet, so nothing was restored", s.Name)
			}
			if only != "" && rep.Downloaded == 0 && rep.SkippedExisting == 0 && len(rep.Failures) == 0 {
				return fmt.Errorf("--only %q matched none of the %s objects stored for %q, so nothing was restored. "+
					"A bare name restores everything under it (--only docs), and \"*\" does not cross a \"/\" "+
					"(use --only 'docs/**')",
					only, progress.FormatCount(rep.ListedFiles), s.Name)
			}
			fmt.Fprintf(opts.Out, "%s: %s restored (%s) into %s in %s\n",
				rep.Set, progress.FormatCount(int64(rep.Downloaded)),
				progress.FormatBytes(rep.Bytes), rep.Target, rep.Elapsed.Round(time.Second))
			if rep.SkippedExisting > 0 {
				fmt.Fprintf(opts.Out, "  %d file(s) already existed and were left alone. Use --overwrite to replace them.\n",
					rep.SkippedExisting)
			}
			if rep.Verified > 0 {
				fmt.Fprintf(opts.Out, "  %d file(s) byte-compared after writing.\n", rep.Verified)
			}
			if n := len(rep.VerifyMismatches); n > 0 {
				fmt.Fprintf(opts.Out, "  %d file(s) did NOT match after writing:\n", n)
				for i, k := range rep.VerifyMismatches {
					if i == 5 {
						break
					}
					fmt.Fprintf(opts.Out, "    %s\n", k)
				}
			}
			if n := len(rep.Failures); n > 0 {
				fmt.Fprintf(opts.Out, "  %d file(s) could not be restored:\n", n)
				for i, f := range rep.Failures {
					if i == 5 {
						break
					}
					fmt.Fprintf(opts.Out, "    %s: %v\n", f.Key, f.Err)
				}
				return fmt.Errorf("%d file(s) did not restore", n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "restore into this directory instead")
	cmd.Flags().StringVar(&only, "only", "",
		`restore only matching paths: a bare name takes everything under it ("docs"), or glob it ("docs/**", "*.pdf") -- "*" does not cross a "/"`)
	cmd.Flags().StringVar(&machine, "machine", "", "restore from another computer's backup")
	cmd.Flags().StringVar(&deleted, "deleted", "", "recover a deleted file from trash")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace files that already exist")
	cmd.Flags().BoolVar(&verify, "verify", false, "re-read and byte-compare each file after writing")
	return cmd
}

// restoreObserver draws the same three-phase progress the backup side does, so
// the two halves of the tool look and behave alike.
type restoreObserver struct {
	out         io.Writer
	set         string
	interactive bool
	lastLines   int
}

func (o *restoreObserver) Phase(p restore.Phase, r *restore.Report) {
	if !o.interactive {
		return
	}
	switch p {
	case restore.PhaseListing:
		fmt.Fprintf(o.out, "[1/3] Listing     %s\n", o.set)
	case restore.PhasePlanning:
		fmt.Fprintf(o.out, "      %s files · %s\n[2/3] Planning\n",
			progress.FormatCount(r.ListedFiles), progress.FormatBytes(r.ListedBytes))
	case restore.PhaseDownloading:
		fmt.Fprintf(o.out, "[3/3] Restoring into %s\n", r.Target)
	}
}

func (o *restoreObserver) Progress(s progress.Snapshot) {
	if !o.interactive {
		return
	}
	for i := 0; i < o.lastLines; i++ {
		fmt.Fprint(o.out, "\033[1A\033[2K")
	}
	line := progress.Render(s, 40)
	fmt.Fprintln(o.out, line)
	o.lastLines = strings.Count(line, "\n") + 1
}
