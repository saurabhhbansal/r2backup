package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/drime"
)

// Choosing a place in Drime by clicking through it: the workspace, then the
// folder. The panel's folder browser and `r2b drime` both use these, for
// where backups go in Drime (the primary backup) and for the Drime folders a
// secondary backup only copies to S3 storage.

// drimeBrowseCreds is the Drime account to browse: where backups go for
// which "primary", otherwise token when one is given, or else the Drime
// account the Drime folders are read with.
func drimeBrowseCreds(a *app, which, token string) (creds.Credentials, error) {
	if which == "primary" {
		c, err := a.creds.Load()
		if err != nil || !c.IsDrime() {
			return creds.Credentials{}, errors.New("backups do not go to Drime on this computer")
		}
		return c, nil
	}
	if t := strings.TrimSpace(token); t != "" {
		c := creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: t}
		// A token typed in the panel talks to the same Drime as the one
		// saved: tests point that at a fake.
		if old, err := drimeSourceCreds(a); err == nil {
			c.Endpoint = old.Endpoint
		}
		return c, nil
	}
	return drimeSourceCreds(a)
}

func drimeConfig(c creds.Credentials, workspace string) drime.Config {
	return drime.Config{Token: c.DrimeToken, WorkspaceID: workspace, BaseURL: c.Endpoint}
}

// drimeFolderList is one folder's subfolders, for the browser.
type drimeFolderList struct {
	Workspace string   `json:"workspace"`
	Path      string   `json:"path"`
	Folders   []string `json:"folders"`
}

func listDrimeFolders(ctx context.Context, c creds.Credentials, workspace, dir string) (drimeFolderList, error) {
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	out := drimeFolderList{Workspace: workspace, Path: dir, Folders: []string{}}
	names, err := drime.Folders(ctx, drimeConfig(c, workspace), dir)
	if errors.Is(err, drime.ErrFolderNotFound) {
		return out, fmt.Errorf("there is no folder %q in Drime any more", dir)
	}
	if err != nil {
		return out, err
	}
	out.Folders = append(out.Folders, names...)
	return out, nil
}

// setDrimeLocation moves where backups go in Drime: another workspace, or
// another folder. What is in the old place stays there.
func setDrimeLocation(ctx context.Context, a *app, workspace, folder string) (creds.Credentials, error) {
	c, err := a.creds.Load()
	if err != nil || !c.IsDrime() {
		return creds.Credentials{}, errors.New("backups do not go to Drime on this computer")
	}
	folder = strings.Trim(strings.TrimSpace(folder), "/")
	if folder == "" {
		return creds.Credentials{}, errors.New("choose a folder: backups do not go at the very top of Drime")
	}
	c.DrimeWorkspace, c.DrimeRoot = strings.TrimSpace(workspace), folder
	if mc, err := a.mirror.Load(); err == nil && samePlace(mc, c) {
		return creds.Credentials{}, errors.New("that is where your secondary backup goes. Choose another folder")
	}
	if err := reachable(ctx, c); err != nil {
		return creds.Credentials{}, err
	}
	return c, a.creds.Save(c)
}

// setDrimeSourceToken changes the Drime API token the Drime folders are read
// with, after checking Drime takes it.
func setDrimeSourceToken(ctx context.Context, a *app, token string) error {
	c, err := drimeBrowseCreds(a, "", token)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("paste the Drime API token")
	}
	if _, err := drime.Workspaces(ctx, drimeConfig(c, "")); err != nil {
		return err
	}
	if old, err := a.source.Load(); err == nil {
		c.DrimeWorkspace = old.DrimeWorkspace
	}
	return a.source.Save(c)
}

// drimeCall serves the panel's folder browser.
func (t *trayBackend) drimeCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	var args struct {
		Which     string `json:"which"` // "primary", or "" for the Drime folders
		Token     string `json:"token"`
		Workspace string `json:"workspace"`
		Path      string `json:"path"`
		Folder    string `json:"folder"`
	}
	switch method {
	case "drimeWorkspaces", "drimeFolders", "setDrimeLocation", "drimeSourceToken":
		if err := decodeArgs(raw, &args); err != nil {
			return nil, true, err
		}
	default:
		return nil, false, nil
	}
	a := t.d.app
	switch method {
	case "drimeWorkspaces":
		c, err := drimeBrowseCreds(a, args.Which, args.Token)
		if err != nil {
			return nil, true, err
		}
		ws, err := drime.Workspaces(ctx, drimeConfig(c, ""))
		return ws, true, err

	case "drimeFolders":
		c, err := drimeBrowseCreds(a, args.Which, args.Token)
		if err != nil {
			return nil, true, err
		}
		res, err := listDrimeFolders(ctx, c, args.Workspace, args.Path)
		return res, true, err

	case "setDrimeLocation":
		c, err := setDrimeLocation(ctx, a, args.Workspace, args.Folder)
		if err != nil {
			return nil, true, err
		}
		t.d.forgetClient()
		t.invalidate()
		_ = t.pushKeys(ctx)
		return describeStore(c), true, nil

	default: // drimeSourceToken
		if err := setDrimeSourceToken(ctx, a, args.Token); err != nil {
			return nil, true, err
		}
		_ = t.pushKeys(ctx)
		return t.drimeSources(), true, nil
	}
}

// newDrimeCmd builds `r2b drime`: the panel's folder browser, as commands.
func newDrimeCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drime",
		Short: "Browse Drime, and choose where backups go there",
		Long: "  r2b drime workspaces                  the Drime workspaces your token reaches\n" +
			"  r2b drime folders [path]              the folders inside a folder in Drime\n" +
			"  r2b drime use <folder>                back up to that folder in Drime\n\n" +
			"--workspace picks a shared workspace (an id from r2b drime workspaces);\n" +
			"leave it out for your own drive. Without --source these read the Drime\n" +
			"backups go to; with it, the Drime the Drime folders are copied from\n" +
			"(r2b mirror drime).",
	}
	var workspace string
	var source bool
	which := func() string {
		if source {
			return ""
		}
		return "primary"
	}

	workspaces := &cobra.Command{
		Use:   "workspaces",
		Short: "The Drime workspaces your token reaches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			c, err := drimeBrowseCreds(a, which(), "")
			if err != nil {
				return err
			}
			ws, err := drime.Workspaces(cmd.Context(), drimeConfig(c, ""))
			if err != nil {
				return err
			}
			for _, w := range ws {
				id := w.ID
				if id == "" {
					id = "(none)"
				}
				mark := ""
				if w.ID == c.DrimeWorkspace && !source {
					mark = "  <- backups go here"
				}
				fmt.Fprintf(opts.Out, "%-10s %s%s\n", id, w.Name, mark)
			}
			return nil
		},
	}
	workspaces.Flags().BoolVar(&source, "source", false, "the Drime the Drime folders are copied from")

	folders := &cobra.Command{
		Use:   "folders [path]",
		Short: "The folders inside a folder in Drime",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			c, err := drimeBrowseCreds(a, which(), "")
			if err != nil {
				return err
			}
			ws := workspace
			if !cmd.Flags().Changed("workspace") && !source {
				ws = c.DrimeWorkspace
			}
			dir := ""
			if len(args) == 1 {
				dir = args[0]
			}
			res, err := listDrimeFolders(cmd.Context(), c, ws, dir)
			if err != nil {
				return err
			}
			if len(res.Folders) == 0 {
				fmt.Fprintln(opts.Out, "No folders in here.")
				return nil
			}
			for _, f := range res.Folders {
				if res.Path != "" {
					f = res.Path + "/" + f
				}
				fmt.Fprintln(opts.Out, f)
			}
			return nil
		},
	}
	folders.Flags().StringVar(&workspace, "workspace", "", "a shared Drime workspace id (default: where backups go, or your own drive)")
	folders.Flags().BoolVar(&source, "source", false, "the Drime the Drime folders are copied from")

	var useWorkspace string
	use := &cobra.Command{
		Use:   "use <folder>",
		Short: "Back up to this folder in Drime (what is in the old one stays)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				a := t.d.app
				old, err := a.creds.Load()
				if err != nil || !old.IsDrime() {
					return errors.New("backups do not go to Drime on this computer")
				}
				ok, err := decide(opts, newPrompter(opts), "Back up somewhere else? New backups go to the new folder. What’s in "+
					destLabel(old)+" stays.\nContinue? [y/N]", false)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintf(opts.Out, "Backups still go to %s.\n", destLabel(old))
					return nil
				}
				c, err := setDrimeLocation(cmd.Context(), a, useWorkspace, args[0])
				if err != nil {
					return err
				}
				_ = t.pushKeys(cmd.Context())
				fmt.Fprintf(opts.Out, "Backups go to %s now.\n", destLabel(c))
				return nil
			})
		},
	}
	use.Flags().StringVar(&useWorkspace, "workspace", "", "a shared Drime workspace id (default: your own drive)")

	cmd.AddCommand(workspaces, folders, use)
	return cmd
}
