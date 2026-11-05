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
		Short: "Keep a second copy of your backups somewhere else",
		Long: "Copies backups from where this computer backs up to a second copy --\n" +
			"Drime to S3 storage (Cloudflare R2, Backblaze B2, Amazon S3, your own),\n" +
			"or the other way -- so there is a copy with a second company. Only\n" +
			"what changed is copied, and files removed from the backup are removed\n" +
			"from the second copy too. Copying back (--reverse) never removes\n" +
			"anything from where backups go.\n\n" +
			"  r2b mirror setup          choose the second destination\n" +
			"  r2b mirror                mirror every set on this computer\n" +
			"  r2b mirror Documents      mirror one set\n" +
			"  r2b mirror --everything   mirror every computer's backups\n" +
			"  r2b mirror --reverse      copy back from the mirror instead\n" +
			"  r2b mirror off            stop keeping a secondary backup\n" +
			"  r2b mirror folder X off   leave one folder out of the secondary backup\n" +
			"  r2b mirror drime ...      copy Drime folders another app backs up to\n" +
			"  r2b mirror ultra on       Ultra fast mode (experimental): Drime to S3 cloud to cloud",
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
	cmd.AddCommand(newMirrorSetupCmd(opts), newMirrorAutoCmd(opts), newMirrorOffCmd(opts), newMirrorFolderCmd(opts),
		newMirrorDrimeCmd(opts), newMirrorUltraCmd(opts))
	return cmd
}

// newMirrorFolderCmd is the Secondary backup switch in the panel's folder
// settings: one folder in or out of the second copy.
func newMirrorFolderCmd(opts *Options) *cobra.Command {
	var del bool
	cmd := &cobra.Command{
		Use:   "folder <set> on|off",
		Short: "Keep one folder in the second copy, or leave it out",
		Long: "Every folder goes to the second copy unless it is turned off here.\n" +
			"Off stops copying it; what the second copy already has of it stays,\n" +
			"unless you add --delete.\n\n" +
			"  r2b mirror folder Photos off            stop copying Photos\n" +
			"  r2b mirror folder Photos off --delete   and delete it from the second copy\n" +
			"  r2b mirror folder Photos on             copy it again",
		Args:      cobra.ExactArgs(2),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			on := strings.EqualFold(args[1], "on")
			if !on && !strings.EqualFold(args[1], "off") {
				return fmt.Errorf("say on or off, not %q", args[1])
			}
			if on && del {
				return errors.New("--delete goes with off")
			}
			return withBackend(opts, func(t *trayBackend) error {
				if del {
					second, err := t.d.app.mirror.Load()
					if err != nil {
						return errors.New("there is no second copy")
					}
					p := newPrompter(opts)
					fmt.Fprintf(p.out, "Delete %s from %s? Your backup of it stays.\n", name, destLabel(second))
					ok, err := decide(opts, p, "Delete? [y/N]", false)
					if err != nil {
						return err
					}
					if !ok {
						fmt.Fprintln(opts.Out, "Nothing changed.")
						return nil
					}
				}
				if _, err := t.Call(cmd.Context(), "folderSecondary",
					mustJSON(map[string]any{"name": name, "on": on, "drop": del})); err != nil {
					return err
				}
				switch {
				case on:
					fmt.Fprintf(opts.Out, "%s goes to the second copy again, from the next copy.\n", name)
				case del:
					fmt.Fprintf(opts.Out, "%s is no longer copied, and is deleted from the second copy.\n", name)
				default:
					fmt.Fprintf(opts.Out, "%s is no longer copied. What’s there stays.\n", name)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&del, "delete", false, "with off: also delete the folder from the second copy")
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
			if provider != "" {
				if _, ok := remote.ParseProvider(provider); !ok {
					return fmt.Errorf("unknown provider %q: use drime, r2, b2, s3 or s3-custom", provider)
				}
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			// S3 storage only has no second copy, as in the panel. Switching
			// to the first purpose sets one up.
			if currentPurpose(a) == purposeS3 && !a.mirror.Exists() {
				return errors.New("this computer is set up for S3 storage only, which has no second copy. " +
					"To back up to Drime with a second copy: r2b setup purpose both")
			}
			return runMirrorSetup(cmd.Context(), a, opts, provider)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "r2, b2, s3 or s3-custom (asked if not given)")
	return cmd
}

func runMirrorSetup(ctx context.Context, a *app, opts *Options, provider string) error {
	p := newPrompter(opts)
	if main, err := a.creds.Load(); err == nil {
		fmt.Fprintf(p.out, "This computer backs up to %s.\n", providerLabel(main))
		if provider == "" {
			fmt.Fprintln(p.out, "A secondary backup on S3 storage puts a copy with a second company.")
		}
	} else {
		fmt.Fprintln(p.out, "This computer has no main destination yet. Run r2b setup first.")
		return err
	}
	if provider == creds.ProviderDrime {
		return errors.New("a secondary backup goes to S3 storage, with a company other than Drime: use r2, b2, s3 or s3-custom")
	}
	c, err := askForS3Destination(ctx, p, provider, "Where should the secondary backup go?")
	if err != nil {
		return err
	}
	fmt.Fprintln(p.out, "Checking the connection...")
	// The panel's own request: refuses the place backups already go, checks
	// the connection, saves it, turns on copying after every backup, and
	// saves the keys to the account. Copying starts by itself, as it does
	// when a second copy is added in the panel.
	t := backendFor(opts, a)
	if err := t.mirrorSetup(ctx, c, true); err != nil {
		return err
	}
	_ = t.pushKeys(ctx) // as after the panel's mirrorSetup (keysChange)
	fmt.Fprintf(p.out, "Second copy: %s. Each backup is copied there once it finishes.\n", destLabel(c))
	fmt.Fprintln(p.out, "To copy everything now: r2b mirror")
	fmt.Fprintln(p.out, "To stop keeping a secondary backup: r2b mirror off")
	return nil
}

func newMirrorAutoCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:       "auto on|off",
		Short:     "Kept for older scripts: the secondary backup is copied after every backup",
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
				fmt.Fprintln(opts.Out, "Copying after every backup.")
			} else {
				fmt.Fprintln(opts.Out, "The secondary backup is still copied after every backup: that is always on now. "+
					"To stop keeping it: r2b mirror off")
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
	defer closeStores(a.client, src, dst)
	if srcName == dstName && src.Bucket() == dst.Bucket() {
		return errors.New("the mirror is the same place as the main destination")
	}
	// The panel's Copy back: every computer's backups, filling in what is
	// missing and removing nothing, after a second press.
	if f.reverse {
		if len(names) == 0 {
			f.everything = true
		}
		if !f.dryRun {
			ok, err := decide(opts, newPrompter(opts), "Copy back from "+srcName+" to "+dstName+
				"? Fills in what’s missing. Removes nothing. [y/N]", false)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(opts.Out, "Nothing copied.")
				return nil
			}
		}
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
	// A folder turned off for the secondary backup is left alone there,
	// unless this is a copy back, which is for getting files back.
	var skip []string
	if !f.reverse {
		if f.everything {
			skip = secondarySkips(a.sets.List())
		}
		for _, s := range list {
			if s.NoSecondary {
				fmt.Fprintf(opts.Out, "%s is kept out of the secondary backup; skipped. Turn it back on: r2b mirror folder %q on\n", s.Name, s.Name)
			}
		}
		list = forSecondary(list)
		if !f.everything && len(list) == 0 {
			fmt.Fprintln(opts.Out, "Nothing to copy.")
			return nil
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
		Src: src, Dst: dst, Prefixes: prefixes, Skip: skip,
		// Copying back never removes anything from where backups go, as in
		// the panel: it is for getting files back, and a second copy that is
		// behind must not be able to take them away.
		Delete: !f.keep && !f.reverse, DryRun: f.dryRun,
		Progress: mirrorProgressPrinter(opts.Out, interactive()),
		Relay:    mirrorRelay(a, f.reverse),
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
	var wasOffline bool
	return func(p mirror.Progress) {
		if time.Since(last) < 250*time.Millisecond && p.FilesDone != p.FilesTotal && p.Offline == wasOffline {
			return
		}
		last, wasOffline = time.Now(), p.Offline
		fmt.Fprintf(out, "\r\033[2K  %d of %d files, %s of %s",
			p.FilesDone, p.FilesTotal, progress.FormatBytes(p.BytesDone), progress.FormatBytes(p.BytesTotal))
		if p.Offline {
			fmt.Fprint(out, " · waiting for the network…")
		}
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
	if !dry {
		for _, l := range relayLines(r) {
			fmt.Fprintln(out, l)
		}
	}
	for i, f := range r.Failures {
		if i == 3 {
			fmt.Fprintf(out, "  ...and %d more\n", len(r.Failures)-3)
			break
		}
		fmt.Fprintf(out, "  %s: %v\n", f.Key, f.Err)
	}
}

// forSecondary is the folders in list that go to the secondary backup: all
// of them but those turned off with `r2b mirror folder <name> off`.
func forSecondary(list []sets.Set) []sets.Set {
	var out []sets.Set
	for _, s := range list {
		if !s.NoSecondary {
			out = append(out, s)
		}
	}
	return out
}

// secondarySkips is the key scope of every folder kept out of the secondary
// backup, for a copy of a wider prefix that would otherwise take them in.
func secondarySkips(list []sets.Set) []string {
	var out []string
	for _, s := range list {
		if s.NoSecondary {
			out = append(out, s.KeyScope())
		}
	}
	return out
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
	// The secondary backup is always copied after a backup. MirrorAuto is
	// still written, for older copies of r2backup, and no longer read here.
	if err != nil || !a.mirror.Exists() || len(done) == 0 {
		return
	}
	var due []sets.Set
	for name, rep := range done {
		changed := rep != nil && rep.Uploaded+rep.Moved+rep.Deleted > 0
		last := time.Unix(st.MirrorLast[name], 0)
		if changed || time.Since(last) > mirrorRecheck {
			if s, err := a.sets.Get(name); err == nil && !s.NoSecondary {
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
	defer closeStores(a.client, src, dst)
	var prefixes []string
	for _, s := range due {
		prefixes = append(prefixes, s.KeyScope())
	}
	rep, err := mirror.Run(ctx, mirror.Options{Src: src, Dst: dst, Prefixes: prefixes, Delete: true, Relay: mirrorRelay(a, false)})
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
