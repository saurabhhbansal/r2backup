package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// mirrorRecheck is how long an automatic mirror trusts that an unchanged set
// is still in step. Once a day it checks anyway, which is what catches a
// file a failed run left behind.
const mirrorRecheck = 24 * time.Hour

func newMirrorCmd(opts *Options) *cobra.Command {
	var (
		reverse, everything, dryRun, keep bool
	)
	cmd := &cobra.Command{
		Use:   "mirror [set...]",
		Short: "Copy your backups to a second place (Drime <-> R2)",
		Long: "Copies backups from where this computer backs up to a second\n" +
			"destination -- Drime to Cloudflare R2, or R2 to Drime -- so there is\n" +
			"a copy with a second company. Only what changed is copied, and\n" +
			"files removed from the backup are removed from the mirror too.\n\n" +
			"  r2b mirror setup          choose the second destination\n" +
			"  r2b mirror                mirror every set on this computer\n" +
			"  r2b mirror Documents      mirror one set\n" +
			"  r2b mirror --everything   mirror every computer's backups\n" +
			"  r2b mirror --reverse      copy back from the mirror instead\n" +
			"  r2b mirror auto on        mirror after every backup, scheduled ones too",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			return runMirror(cmd.Context(), a, opts, args, mirrorFlags{
				reverse: reverse, everything: everything, dryRun: dryRun, keep: keep,
			})
		},
	}
	cmd.Flags().BoolVar(&reverse, "reverse", false, "copy from the mirror to the main destination")
	cmd.Flags().BoolVar(&everything, "everything", false, "mirror every computer's backups, not only this one's sets")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be copied and removed, and change nothing")
	cmd.Flags().BoolVar(&keep, "keep", false, "never remove anything from the destination")
	cmd.AddCommand(newMirrorSetupCmd(opts), newMirrorAutoCmd(opts))
	return cmd
}

type mirrorFlags struct {
	reverse, everything, dryRun, keep bool
}

func newMirrorSetupCmd(opts *Options) *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Choose where `r2b mirror` copies to",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() && opts.In == nil {
				return errors.New("mirror setup needs a terminal")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			return runMirrorSetup(cmd.Context(), a, opts, provider)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "drime or r2 (asked if not given)")
	return cmd
}

func runMirrorSetup(ctx context.Context, a *app, opts *Options, provider string) error {
	p := newPrompter(opts)
	if main, err := a.creds.Load(); err == nil {
		fmt.Fprintf(p.out, "This computer backs up to %s.\n", providerLabel(main))
		if provider == "" {
			if main.IsDrime() {
				fmt.Fprintln(p.out, "A mirror on Cloudflare R2 would put a second copy with a second company.")
			} else {
				fmt.Fprintln(p.out, "A mirror on Drime would put a second copy with a second company.")
			}
		}
	} else {
		fmt.Fprintln(p.out, "This computer has no main destination yet. Run r2b setup first.")
		return err
	}
	c, err := askForDestination(ctx, p, provider)
	if err != nil {
		return err
	}
	fmt.Fprintln(p.out, "Checking the connection...")
	store, err := openStore(ctx, c, nil)
	if err != nil {
		return err
	}
	if _, err := store.List(ctx, "r2backup/"); err != nil {
		return fmt.Errorf("could not read the mirror destination: %w", err)
	}
	if err := a.mirror.Save(c); err != nil {
		return err
	}
	fmt.Fprintf(p.out, "Mirror set to %s (%s).\n", providerLabel(c), store.Bucket())

	auto, err := confirm(p, "Mirror automatically after each backup? [Y/n]", true)
	if err != nil {
		return err
	}
	if err := setMirrorAuto(auto); err != nil {
		return err
	}
	if auto {
		fmt.Fprintln(p.out, "On. Each backup will be copied there once it finishes.")
	}
	fmt.Fprintln(p.out, "To copy everything now: r2b mirror")
	return nil
}

func newMirrorAutoCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:       "auto on|off",
		Short:     "Mirror after every backup, or stop",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			on := strings.EqualFold(args[0], "on")
			if !on && !strings.EqualFold(args[0], "off") {
				return fmt.Errorf("say on or off, not %q", args[0])
			}
			if on {
				a, err := openApp()
				if err != nil {
					return err
				}
				exists := a.mirror.Exists()
				a.close()
				if !exists {
					return errors.New("no mirror destination yet. Run: r2b mirror setup")
				}
			}
			if err := setMirrorAuto(on); err != nil {
				return err
			}
			if on {
				fmt.Fprintln(opts.Out, "Backups will be mirrored after each run.")
			} else {
				fmt.Fprintln(opts.Out, "Automatic mirroring is off. r2b mirror still works by hand.")
			}
			return nil
		},
	}
}

func setMirrorAuto(on bool) error {
	s, err := config.LoadSettings()
	if err != nil {
		return err
	}
	s.MirrorAuto = on
	return config.SaveSettings(s)
}

// mirrorEnds connects both destinations and returns them in copy order.
func mirrorEnds(ctx context.Context, a *app, reverse bool) (src, dst remote.Store, srcName, dstName string, err error) {
	mc, err := a.mirror.Load()
	if err != nil {
		if errors.Is(err, creds.ErrNotFound) {
			return nil, nil, "", "", errors.New("no mirror destination yet. Run: r2b mirror setup")
		}
		return nil, nil, "", "", err
	}
	if err := a.connect(ctx); err != nil {
		return nil, nil, "", "", err
	}
	mainCreds, _ := a.creds.Load()
	other, err := openStore(ctx, mc, nil)
	if err != nil {
		return nil, nil, "", "", err
	}
	src, dst = a.client, other
	srcName, dstName = providerLabel(mainCreds), providerLabel(mc)
	if reverse {
		src, dst, srcName, dstName = dst, src, dstName, srcName
	}
	return src, dst, srcName, dstName, nil
}

func runMirror(ctx context.Context, a *app, opts *Options, names []string, f mirrorFlags) error {
	src, dst, srcName, dstName, err := mirrorEnds(ctx, a, f.reverse)
	if err != nil {
		return err
	}
	if srcName == dstName && src.Bucket() == dst.Bucket() {
		return errors.New("the mirror is the same place as the main destination")
	}

	var prefixes []string
	var list []sets.Set
	switch {
	case f.everything:
		if len(names) > 0 {
			return errors.New("give set names or --everything, not both")
		}
		prefixes = []string{"machines/"}
	case len(names) == 0:
		list, err = a.resolveSets("")
		if err != nil {
			return err
		}
	default:
		for _, n := range names {
			s, err := a.sets.Get(n)
			if err != nil {
				return err
			}
			list = append(list, s)
		}
	}
	for _, s := range list {
		prefixes = append(prefixes, s.KeyScope())
	}

	verb := "Mirroring"
	if f.dryRun {
		verb = "Checking"
	}
	fmt.Fprintf(opts.Out, "%s %s -> %s (%s)\n", verb, srcName, dstName, dst.Bucket())
	rep, runErr := mirror.Run(ctx, mirror.Options{
		Src: src, Dst: dst, Prefixes: prefixes,
		Delete: !f.keep, DryRun: f.dryRun,
		Progress: mirrorProgressPrinter(opts.Out, interactive()),
	})
	if rep == nil {
		return runErr
	}
	if interactive() {
		fmt.Fprint(opts.Out, "\r\033[2K")
	}
	printMirrorReport(opts.Out, rep, f.dryRun)
	if runErr == nil && len(rep.Failures) == 0 && !f.dryRun && !f.reverse {
		recordMirrored(list, f.everything)
	}
	if runErr != nil {
		return runErr
	}
	if len(rep.Failures) > 0 {
		return fmt.Errorf("%d file(s) did not copy; the next run tries them again", len(rep.Failures))
	}
	return nil
}

func mirrorProgressPrinter(out io.Writer, tty bool) func(mirror.Progress) {
	if !tty {
		return nil
	}
	var last time.Time
	return func(p mirror.Progress) {
		if time.Since(last) < 250*time.Millisecond && p.FilesDone != p.FilesTotal {
			return
		}
		last = time.Now()
		fmt.Fprintf(out, "\r\033[2K  %d of %d files, %s of %s",
			p.FilesDone, p.FilesTotal, progress.FormatBytes(p.BytesDone), progress.FormatBytes(p.BytesTotal))
	}
}

func printMirrorReport(out io.Writer, r *mirror.Report, dry bool) {
	if dry {
		fmt.Fprintf(out, "Would copy %d file(s), %s. Would remove %d. %d already in step.\n",
			r.Copied, progress.FormatBytes(r.Bytes), r.Deleted, r.Unchanged)
	} else {
		fmt.Fprintf(out, "Copied %d file(s), %s. Removed %d. %d already in step. Took %s.\n",
			r.Copied, progress.FormatBytes(r.Bytes), r.Deleted, r.Unchanged, r.Elapsed.Round(time.Second))
	}
	if r.SkippedDeletes > 0 {
		fmt.Fprintf(out, "Left %d file(s) on the mirror that the source does not list: the source looked empty,\n"+
			"which is more often a wrong account or an outage than a real deletion.\n", r.SkippedDeletes)
	}
	for i, f := range r.Failures {
		if i == 3 {
			fmt.Fprintf(out, "  ...and %d more\n", len(r.Failures)-3)
			break
		}
		fmt.Fprintf(out, "  %s: %v\n", f.Key, f.Err)
	}
}

func recordMirrored(list []sets.Set, everything bool) {
	s, err := config.LoadSettings()
	if err != nil {
		return
	}
	if s.MirrorLast == nil {
		s.MirrorLast = map[string]int64{}
	}
	now := time.Now().Unix()
	if everything {
		s.MirrorLast["*"] = now
	}
	for _, set := range list {
		s.MirrorLast[set.Name] = now
	}
	_ = config.SaveSettings(s)
}

// autoMirror runs after `r2b backup`, scheduled or not, when mirroring is on.
// A set is mirrored when its backup changed something or when it has not been
// checked for a day. It reports to out and never fails the backup: the
// backup itself already succeeded, and a mirror that could not run is said
// so and tried again next time.
func autoMirror(ctx context.Context, a *app, out io.Writer, done map[string]*backup.Report) {
	st, err := config.LoadSettings()
	if err != nil || !st.MirrorAuto || !a.mirror.Exists() || len(done) == 0 {
		return
	}
	var due []sets.Set
	for name, rep := range done {
		changed := rep != nil && rep.Uploaded+rep.Moved+rep.Deleted > 0
		last := time.Unix(st.MirrorLast[name], 0)
		if changed || time.Since(last) > mirrorRecheck {
			if s, err := a.sets.Get(name); err == nil {
				due = append(due, s)
			}
		}
	}
	if len(due) == 0 {
		return
	}
	src, dst, srcName, dstName, err := mirrorEnds(ctx, a, false)
	if err != nil {
		fmt.Fprintf(out, "Mirror: %v\n", err)
		return
	}
	var prefixes []string
	for _, s := range due {
		prefixes = append(prefixes, s.KeyScope())
	}
	rep, err := mirror.Run(ctx, mirror.Options{Src: src, Dst: dst, Prefixes: prefixes, Delete: true})
	if rep == nil {
		fmt.Fprintf(out, "Mirror to %s did not run: %v\n", dstName, err)
		return
	}
	fmt.Fprintf(out, "Mirror %s -> %s: ", srcName, dstName)
	printMirrorReport(out, rep, false)
	if err == nil && len(rep.Failures) == 0 {
		recordMirrored(due, false)
	}
}
