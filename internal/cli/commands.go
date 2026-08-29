package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/scan"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/trash"
	"github.com/saurabhhbansal/r2backup/internal/tui"
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
			if _, err := a.sets.Get(name); err == nil {
				return fmt.Errorf("a set called %q already exists; pass --name to choose another", name)
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
			if retention < 0 {
				s.RetentionDays = sets.DefaultRetentionDays
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
			fmt.Fprintf(opts.Out, "\nTo have this run by itself: r2backup schedule --every %dm\n", stored.IntervalMinutes)
			return nil
		},
	}
	cmd.Flags().IntVar(&interval, "every", sets.DefaultIntervalMinutes, "minutes between scheduled runs")
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
		fmt.Fprintln(opts.Out, "Not scheduled. To run automatically: r2backup schedule --every 30m")
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

func newScheduleCmd(opts *Options) *cobra.Command {
	var (
		every  int
		remove bool
	)
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Register with the operating system's scheduler",
		Long: "Registers a hidden Task Scheduler entry on Windows, a systemd user\n" +
			"timer on Linux, or a launchd agent on macOS. Nothing of ours stays\n" +
			"running in between, so updating the binary is never blocked.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !schedule.Supported() {
				return errors.New("no scheduler is available on this platform")
			}
			if remove {
				if err := schedule.Remove("r2backup"); err != nil {
					return err
				}
				fmt.Fprintln(opts.Out, "Unregistered. Backups will only run when you run them.")
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
				Args:       []string{"backup"},
			}); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Registered. Backups run every %d minutes, hidden, and survive a reboot.\n", every)
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
			if err := a.sets.Rename(args[0], args[1]); err != nil {
				return err
			}
			n, _ := a.index.Count(args[1])
			fmt.Fprintf(opts.Out, "Renamed to %q.\n", args[1])
			fmt.Fprintf(opts.Out,
				"The bucket still stores it under %q. Moving it would copy %s objects,\n"+
					"one operation each, for a cosmetic change.\n", s.Prefix, progress.FormatCount(int64(n)))
			if remote {
				return errors.New("--remote is not implemented yet")
			}
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
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return notYet("restore") },
	}
	cmd.Flags().StringVar(&to, "to", "", "restore into this directory instead")
	cmd.Flags().StringVar(&only, "only", "", "restore only paths matching this glob")
	cmd.Flags().StringVar(&machine, "machine", "", "restore from another computer's backup")
	cmd.Flags().StringVar(&deleted, "deleted", "", "recover a deleted file from trash")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace files that already exist")
	cmd.Flags().BoolVar(&verify, "verify", false, "re-download and byte-compare afterwards")
	return cmd
}
