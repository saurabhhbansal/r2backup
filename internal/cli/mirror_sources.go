package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// Copying Drime folders to S3 storage, for someone who backs up to Drime with
// another app and wants a second copy with a second company without changing
// how they back up. r2backup reads the folder through the Drime API and
// mirrors it -- only what changed, removals included -- to the S3-compatible
// destination this computer has.
//
// On the destination each folder lands under
//
//	machines/drime-mirror/<name>/current/
//
// which is where r2backup keeps a backup, so it can be browsed and restored
// like one (`r2b restore <name> --machine drime-mirror`), and bundled when the
// destination bundles.

// drimeMirrorMachine is the machine name Drime folders are filed under.
const drimeMirrorMachine = "drime-mirror"

// drimeSourceRecheck is how often an automatic copy runs at most.
const drimeSourceRecheck = 6 * time.Hour

func drimeSourcePrefix(name string) string {
	return "machines/" + drimeMirrorMachine + "/" + name + "/current/"
}

func validSourceName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.TrimSpace(name) != name {
		return fmt.Errorf("%q will not do as a name: no slashes, and no spaces at either end", name)
	}
	return nil
}

// s3Destination is where Drime folders are copied: this computer's main
// destination when it is S3-compatible, otherwise its second copy.
func s3Destination(a *app) (creds.Credentials, error) {
	if c, err := a.creds.Load(); err == nil && c.IsS3() {
		return c, nil
	}
	if c, err := a.mirror.Load(); err == nil && c.IsS3() {
		return c, nil
	}
	return creds.Credentials{}, errors.New("add S3-compatible storage first (Cloudflare R2, Backblaze B2, Amazon S3 or your own endpoint): the Drime folders are copied there")
}

// drimeSourceCreds is the Drime account to read from: the one saved for this
// purpose, or else whichever of this computer's destinations is Drime.
func drimeSourceCreds(a *app) (creds.Credentials, error) {
	if c, err := a.source.Load(); err == nil {
		return c, nil
	}
	for _, s := range []*creds.Store{a.creds, a.mirror} {
		if c, err := s.Load(); err == nil && c.IsDrime() {
			return c, nil
		}
	}
	return creds.Credentials{}, errors.New("no Drime API token on this computer yet")
}

// drimeSourceEnds opens one source folder and its place on the destination.
func drimeSourceEnds(ctx context.Context, a *app, src config.MirrorSource) (from, to remote.Store, err error) {
	dc, err := drimeSourceCreds(a)
	if err != nil {
		return nil, nil, err
	}
	dc.DrimeRoot = src.Folder
	if src.Workspace != "" {
		dc.DrimeWorkspace = src.Workspace
	}
	dc.Bundle = false
	from, err = openBaseStore(ctx, dc, nil)
	if err != nil {
		return nil, nil, err
	}
	dest, err := s3Destination(a)
	if err != nil {
		return nil, nil, err
	}
	store, err := openStore(ctx, dest, nil)
	if err != nil {
		return nil, nil, err
	}
	return from, remote.Sub(store, drimeSourcePrefix(src.Name)), nil
}

// addDrimeSource records a Drime folder to copy, saving token as the Drime
// account to read from when one is given. The folder is checked first.
func addDrimeSource(ctx context.Context, a *app, token string, src config.MirrorSource) error {
	src.Folder = strings.Trim(strings.TrimSpace(src.Folder), "/")
	if src.Name == "" {
		src.Name = src.Folder[strings.LastIndex(src.Folder, "/")+1:]
	}
	if err := validSourceName(src.Name); err != nil {
		return err
	}
	if src.Folder == "" {
		return errors.New("choose the Drime folder to copy")
	}
	if token = strings.TrimSpace(token); token != "" {
		c := creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: token, DrimeWorkspace: src.Workspace}
		if err := a.source.Save(c); err != nil {
			return err
		}
	}
	if _, err := s3Destination(a); err != nil {
		return err
	}
	from, _, err := drimeSourceEnds(ctx, a, src)
	if err != nil {
		return err
	}
	if _, err := from.List(ctx, ""); err != nil {
		return fmt.Errorf("could not read %q in Drime: %w", src.Folder, err)
	}
	st, err := config.LoadSettings()
	if err != nil {
		return err
	}
	for i, s := range st.MirrorSources {
		if s.Name == src.Name {
			st.MirrorSources[i] = src
			return config.SaveSettings(st)
		}
	}
	st.MirrorSources = append(st.MirrorSources, src)
	return config.SaveSettings(st)
}

// removeDrimeSource stops copying a folder. What was copied stays on the
// destination.
func removeDrimeSource(name string) error {
	st, err := config.LoadSettings()
	if err != nil {
		return err
	}
	for i, s := range st.MirrorSources {
		if s.Name == name {
			st.MirrorSources = append(st.MirrorSources[:i], st.MirrorSources[i+1:]...)
			return config.SaveSettings(st)
		}
	}
	return fmt.Errorf("no Drime folder called %q is being copied", name)
}

// drimeSourceResult is how copying one folder went.
type drimeSourceResult struct {
	Name   string
	Report *mirror.Report
	Err    error
}

// runDrimeSources copies the named folders (all of them when names is empty).
// due, when set, picks which of them to run.
func runDrimeSources(ctx context.Context, a *app, names []string, dry bool, due func(config.MirrorSource) bool,
	progress func(name string, p mirror.Progress)) ([]drimeSourceResult, error) {
	st, err := config.LoadSettings()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []drimeSourceResult
	for _, src := range st.MirrorSources {
		if len(want) > 0 && !want[src.Name] {
			continue
		}
		delete(want, src.Name)
		if due != nil && !due(src) {
			continue
		}
		res := drimeSourceResult{Name: src.Name}
		from, to, err := drimeSourceEnds(ctx, a, src)
		if err == nil {
			opts := mirror.Options{Src: from, Dst: to, Prefixes: []string{""}, Delete: true, DryRun: dry}
			if progress != nil {
				name := src.Name
				opts.Progress = func(p mirror.Progress) { progress(name, p) }
			}
			res.Report, err = mirror.Run(ctx, opts)
		}
		res.Err = err
		out = append(out, res)
		if err == nil && !dry && res.Report != nil && len(res.Report.Failures) == 0 {
			markDrimeSourceRun(src.Name)
		}
	}
	for n := range want {
		return out, fmt.Errorf("no Drime folder called %q is being copied", n)
	}
	return out, nil
}

func markDrimeSourceRun(name string) {
	st, err := config.LoadSettings()
	if err != nil {
		return
	}
	for i := range st.MirrorSources {
		if st.MirrorSources[i].Name == name {
			st.MirrorSources[i].LastRun = time.Now().Unix()
		}
	}
	_ = config.SaveSettings(st)
}

// autoDrimeSources runs after a scheduled backup: the folders set to copy
// automatically, each at most every few hours.
func autoDrimeSources(ctx context.Context, a *app, out io.Writer) {
	due := func(s config.MirrorSource) bool {
		return s.Auto && time.Since(time.Unix(s.LastRun, 0)) > drimeSourceRecheck
	}
	results, _ := runDrimeSources(ctx, a, nil, false, due, nil)
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(out, "Drime folder %s: %v\n", r.Name, r.Err)
			continue
		}
		fmt.Fprintf(out, "Drime folder %s: ", r.Name)
		printMirrorReport(out, r.Report, false)
	}
}

func newMirrorDrimeCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drime",
		Short: "Copy Drime folders another app backs up to, to S3 storage",
		Long: "For backups made to Drime with another app: r2backup copies the\n" +
			"folders to this computer's S3-compatible storage, keeping the copy\n" +
			"exact. Nothing about how you back up to Drime changes.\n\n" +
			"  r2b mirror drime add Backups/Laptop    copy a folder\n" +
			"  r2b mirror drime ls                    what is being copied\n" +
			"  r2b mirror drime run                   copy now\n" +
			"  r2b mirror drime rm Laptop             stop copying it",
	}

	var name, workspace string
	var auto bool
	add := &cobra.Command{
		Use:   "add <drime-folder>",
		Short: "Copy a Drime folder to S3 storage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			var token string
			if _, err := drimeSourceCreds(a); err != nil {
				if !interactive() && opts.In == nil {
					return errors.New("no Drime API token yet; run this in a terminal to enter one")
				}
				fmt.Fprintln(opts.Out, "Make a Drime API token: Drime > Settings > Developer > Create token.")
				if token, err = newPrompter(opts).secret("Drime API token"); err != nil {
					return err
				}
			}
			src := config.MirrorSource{Name: name, Folder: args[0], Workspace: workspace, Auto: auto}
			if err := addDrimeSource(cmd.Context(), a, token, src); err != nil {
				return err
			}
			dest, _ := s3Destination(a)
			fmt.Fprintf(opts.Out, "Copying %s to %s. To copy it now: r2b mirror drime run\n", args[0], destLabel(dest))
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "", "what to call it (default: the folder's own name)")
	add.Flags().StringVar(&workspace, "workspace", "", "a shared Drime workspace id (default: your own drive)")
	add.Flags().BoolVar(&auto, "auto", true, "copy it after every scheduled backup")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "The Drime folders being copied",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := config.LoadSettings()
			if err != nil {
				return err
			}
			if len(st.MirrorSources) == 0 {
				fmt.Fprintln(opts.Out, "No Drime folders are being copied. Add one: r2b mirror drime add <folder>")
				return nil
			}
			for _, s := range st.MirrorSources {
				last := "never"
				if s.LastRun > 0 {
					last = time.Unix(s.LastRun, 0).Format("2 Jan 15:04")
				}
				mode := "by hand"
				if s.Auto {
					mode = "automatically"
				}
				fmt.Fprintf(opts.Out, "%-20s %-30s %s, last copied %s\n", s.Name, s.Folder, mode, last)
			}
			return nil
		},
	}

	var dry bool
	run := &cobra.Command{
		Use:   "run [name...]",
		Short: "Copy the Drime folders now",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			results, err := runDrimeSources(cmd.Context(), a, args, dry, nil, nil)
			for _, r := range results {
				if r.Err != nil {
					fmt.Fprintf(opts.Err, "%s: %v\n", r.Name, r.Err)
					continue
				}
				fmt.Fprintf(opts.Out, "%s: ", r.Name)
				printMirrorReport(opts.Out, r.Report, dry)
			}
			return err
		},
	}
	run.Flags().BoolVar(&dry, "dry-run", false, "say what would be copied and removed, and change nothing")

	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Stop copying a Drime folder (what was copied stays)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := removeDrimeSource(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Stopped copying %s. What was copied is still there.\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, ls, run, rm)
	return cmd
}
