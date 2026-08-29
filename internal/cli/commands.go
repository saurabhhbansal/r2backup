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
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/progress"
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

func newSetupCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Store this machine's Cloudflare R2 credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()

			if !interactive() {
				return errors.New("setup needs a terminal")
			}
			in := bufio.NewReader(os.Stdin)
			ask := func(label string) (string, error) {
				fmt.Fprintf(opts.Out, "%s: ", label)
				line, err := in.ReadString('\n')
				return strings.TrimSpace(line), err
			}
			c := creds.Credentials{}
			for _, f := range []struct {
				label string
				into  *string
			}{
				{"Cloudflare account ID", &c.AccountID},
				{"R2 access key ID", &c.AccessKeyID},
				{"R2 bucket name", &c.Bucket},
			} {
				v, err := ask(f.label)
				if err != nil {
					return err
				}
				*f.into = v
			}
			fmt.Fprint(opts.Out, "R2 secret access key: ")
			secret, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(opts.Out)
			if err != nil {
				return err
			}
			c.SecretAccessKey = strings.TrimSpace(string(secret))

			if err := a.creds.Save(c); err != nil {
				return err
			}
			name, protected := a.creds.Protection()
			fmt.Fprintf(opts.Out, "\nSaved. Secret is guarded by %s.\n", name)
			if !protected {
				fmt.Fprintln(opts.Out, "Note: no OS keystore is available here, so this is file permissions only.")
			}
			fmt.Fprintln(opts.Out, "Checking the connection...")
			if err := a.connect(cmd.Context()); err != nil {
				return err
			}
			if _, err := a.client.List(cmd.Context(), "r2backup/"); err != nil {
				return fmt.Errorf("credentials saved, but the bucket could not be read: %w", err)
			}
			fmt.Fprintln(opts.Out, "Connection works. Next: r2backup add <folder>")
			return nil
		},
	}
}

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
				ex, accepted, err := tui.Pick(root, scanned)
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
				Prefix:          "machines/" + machineName() + "/" + name,
				Excludes:        excludes,
				RetentionDays:   retention,
				IntervalMinutes: interval,
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
			fmt.Fprintf(opts.Out, "\nTo have this run by itself: r2backup schedule --every %d\n", stored.IntervalMinutes)
			return nil
		},
	}
	cmd.Flags().IntVar(&interval, "every", sets.DefaultIntervalMinutes,
		"how often this set wants to run, in minutes: recorded, and used in the suggestion printed after adding. Nothing is scheduled until you run r2backup schedule")
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
				if err != nil {
					failed++
					fmt.Fprintf(opts.Err, "%s: %v\n", s.Name, err)
					// A vanished folder is never a deletion. Park the set and
					// leave the bucket untouched until a person decides.
					if errors.Is(err, backup.ErrRootMissing) {
						_ = a.sets.MarkNeedsAttention(s.Name, err.Error())
						fmt.Fprintf(opts.Err,
							"  Nothing was deleted. If the folder moved: r2backup relink %q <new-path>\n", s.Name)
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

func newStatusCmd(opts *Options) *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "What ran, when, and what is next",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if watch {
				return watchProgress(cmd.Context(), opts)
			}
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
		fmt.Fprintln(opts.Out, "Nothing is being backed up yet. Run: r2backup add <folder>")
		return nil
	}
	histPath, _ := historyPath()
	hist, _ := runstate.ReadHistory(histPath)

	progressPath, _ := config.ProgressPath()
	if live, err := runstate.ReadLive(progressPath); err == nil && !live.Stale(time.Now()) {
		fmt.Fprintf(opts.Out, "Running now: %s — %s of %s, %s\n\n", live.Set,
			progress.FormatBytes(live.BytesDone), progress.FormatBytes(live.BytesTotal),
			etaText(live))
	}

	for _, s := range list {
		fmt.Fprintf(opts.Out, "%s\n  %s\n", s.Name, s.Root)
		if s.Status == sets.StatusNeedsAttention {
			fmt.Fprintf(opts.Out, "  NEEDS ATTENTION: %s\n", s.StatusNote)
		}
		if last, ok := hist.Last(s.Name); ok {
			when := humanAgo(time.Since(last.FinishedAt))
			if last.Error != "" {
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
	if used, resetAt, err := a.index.OpsThisMonth(); err == nil {
		fmt.Fprintf(opts.Out, "Operations this month: %s of %s free (resets %s)\n",
			progress.FormatCount(int64(used)),
			progress.FormatCount(int64(index.FreeTierOpsPerMonth)),
			resetAt.Format("2 Jan"))
	}
	if st, err := schedule.Current("r2backup"); err == nil && st.Registered {
		fmt.Fprintf(opts.Out, "Scheduled: every %s\n", st.Interval)
	} else {
		fmt.Fprintln(opts.Out, "Not scheduled. To run automatically: r2backup schedule --every 30")
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
			for _, s := range list {
				n, err := a.index.Count(s.Name)
				if err != nil {
					return err
				}
				fmt.Fprintf(opts.Out, "%s: %s objects\n", s.Name, progress.FormatCount(int64(n)))
				if only != "" {
					recs, err := a.index.All(s.Name)
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
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if err := schedule.Install(schedule.Entry{
				Name:       "r2backup",
				Interval:   time.Duration(every) * time.Minute,
				BinaryPath: self,
				Args:       scheduledRunArgs(runtime.GOOS),
			}); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Registered. Backups run every %d minutes, out of sight, and survive a reboot.\n", every)
			// On Windows the preferred registration can be refused and a
			// second one used instead, and the difference matters: one runs
			// whether or not you are signed in, the other does not. Read it
			// back rather than claiming whichever was asked for first.
			if st, err := schedule.Current("r2backup"); err == nil && st.Registered && !st.RunsWhenSignedOut && runtime.GOOS == "windows" {
				fmt.Fprintln(opts.Out,
					"Note: it runs while you are signed in. Windows would not grant the\n"+
						"      permission needed to run it when you are signed out, which\n"+
						"      needs the \"Log on as a batch job\" right for your account.")
			}
			fmt.Fprintln(opts.Out, "To watch one: r2backup status --watch")
			return nil
		},
	}
	cmd.Flags().IntVar(&every, "every", sets.DefaultIntervalMinutes, "minutes between runs")
	cmd.Flags().BoolVar(&remove, "remove", false, "unregister instead")
	return cmd
}

func newRenameCmd(opts *Options) *cobra.Command {
	var remote bool
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
			// Checked before anything is written. It used to be checked after
			// the rename had already happened, so `rename x y --remote`
			// renamed the set, printed that it had, and then exited 1 -- a
			// command that both succeeded and failed.
			if remote {
				n, _ := a.index.Count(args[0])
				return fmt.Errorf("--remote is not implemented yet, and nothing has been changed. "+
					"It would copy %s objects from %q to a new prefix and delete the originals, "+
					"one operation each way, for a change only you can see",
					progress.FormatCount(int64(n)), s.Prefix)
			}
			// The index is keyed by set name too. Move it first: it is one
			// bbolt transaction and so cannot half-happen, and if the set
			// store then refuses the new name the index goes back where it
			// was. The other order has no recovery -- it is what left a
			// renamed set reading an empty index and re-uploading everything.
			if err := a.index.RenameSet(args[0], args[1]); err != nil {
				return err
			}
			if err := a.sets.Rename(args[0], args[1]); err != nil {
				if back := a.index.RenameSet(args[1], args[0]); back != nil {
					return fmt.Errorf("%w (and the index could not be put back under %q: %v -- "+
						"run `r2backup backup %s` to rebuild it, which re-uploads the set)",
						err, args[0], back, args[1])
				}
				return err
			}
			n, _ := a.index.Count(args[1])
			fmt.Fprintf(opts.Out, "Renamed to %q.\n", args[1])
			fmt.Fprintf(opts.Out,
				"The bucket still stores it under %q. Moving it would copy %s objects,\n"+
					"one operation each, for a cosmetic change.\n", s.Prefix, progress.FormatCount(int64(n)))
			return nil
		},
	}
	cmd.Flags().BoolVar(&remote, "remote", false,
		"also move the objects in the bucket, at one operation each")
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
			s, err := a.sets.Get(args[0])
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
					return fmt.Errorf("%w\n  The original folder %q is not on this machine.\n"+
						"  Say where to put it: r2backup restore %q --to <directory>", err, s.Root, s.Name)
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
						"Check the name with `r2backup account devices`", s.Name, machine)
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
