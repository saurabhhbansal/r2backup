package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// The panel's Backups screen, its trash, packing per destination and the
// usage and cost screen, for the terminal. Each command goes through the
// trayBackend method the panel's request does.

// whereToDelete asks the panel's "delete it from where?" question, when there
// is a second copy to ask about. Both is the default, as in the panel.
func whereToDelete(opts *Options, p *prompter, t *trayBackend, keepSecond bool) (deleteWhere, error) {
	second, err := t.d.app.mirror.Load()
	if err != nil {
		return wherePrimary, nil // no second copy
	}
	if keepSecond {
		return wherePrimary, nil
	}
	if opts.Decision() != Ask {
		return whereBoth, nil
	}
	primary := primaryLabel(t)
	fmt.Fprintln(p.out, "Delete it from:")
	fmt.Fprintf(p.out, "  1. Both                 %s and %s\n", primary, destLabel(second))
	fmt.Fprintf(p.out, "  2. Only %s   The second copy keeps it\n", primary)
	for {
		v, err := p.ask("Choose 1 or 2 (enter for both)")
		if err != nil {
			return "", err
		}
		switch strings.TrimSpace(v) {
		case "", "1":
			return whereBoth, nil
		case "2":
			return wherePrimary, nil
		}
		fmt.Fprintln(p.out, "  Enter 1 or 2.")
	}
}

func primaryLabel(t *trayBackend) string {
	if c, err := t.d.app.creds.Load(); err == nil {
		return destLabel(c)
	}
	return "your storage"
}

// confirmDelete asks the panel's "Delete …?" question.
func confirmDelete(opts *Options, p *prompter, title, text string) error {
	fmt.Fprintf(p.out, "%s\n%s\n", title, text)
	ok, err := decide(opts, p, "Delete? [y/N]", false)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("nothing was deleted")
	}
	return nil
}

func newBackupsCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backups",
		Short: "Every backup in your storage, by computer",
		Long: "Lists every backup in your storage, this computer's first, then each\n" +
			"other computer's. Restore one with r2b restore <name> (add --machine\n" +
			"for another computer's); delete one no folder here uses any more with\n" +
			"r2b backups delete <name>.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				return printBackups(cmd.Context(), t, opts.Out)
			})
		},
	}

	var machine string
	var keepSecond bool
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a backup no folder on this computer uses",
		Long: "Deletes every version of a backup another computer made, or one left\n" +
			"behind when a folder stopped being backed up. This cannot be undone.\n" +
			"With a second copy, it is deleted there too unless you pass\n" +
			"--keep-second-copy.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				ctx := cmd.Context()
				row, err := findStoredBackup(ctx, t, args[0], machine)
				if err != nil {
					return err
				}
				p := newPrompter(opts)
				if err := confirmDelete(opts, p, "Delete "+row.Name+"?", "Every version is deleted. This can’t be undone."); err != nil {
					return err
				}
				where, err := whereToDelete(opts, p, t, keepSecond)
				if err != nil {
					return err
				}
				if why := t.busyReason(); why != "" {
					return fmt.Errorf("%s is running. Try again when it has finished", why)
				}
				msg, err := t.deleteStoredBackup(ctx, row.Machine, row.Name, where)
				if msg != "" {
					fmt.Fprintln(opts.Out, msg)
				}
				return err
			})
		},
	}
	del.Flags().StringVar(&machine, "machine", "", "the computer that made it, when two have one by that name")
	del.Flags().BoolVar(&keepSecond, "keep-second-copy", false, "delete it only where backups go; the second copy keeps it")
	cmd.AddCommand(del)
	return cmd
}

// printBackups groups the panel's restoreSets the way its Backups screen does.
func printBackups(ctx context.Context, t *trayBackend, out io.Writer) error {
	rows, err := t.Call(ctx, "restoreSets", nil)
	if err != nil {
		return err
	}
	list := rows.([]restoreSetOut)
	if len(list) == 0 {
		fmt.Fprintln(out, "No backups yet.")
		return nil
	}
	var order []string
	groups := map[string][]restoreSetOut{}
	for _, x := range list {
		g := x.Machine
		if x.Here || x.Mine {
			g = "This computer"
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], x)
	}
	// This computer's first, as in the panel.
	for i, g := range order {
		if g == "This computer" && i > 0 {
			order = append([]string{g}, append(order[:i:i], order[i+1:]...)...)
			break
		}
	}
	for i, g := range order {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, g)
		for _, x := range groups[g] {
			sub := ""
			switch {
			case x.Here:
				sub = x.Root
			case x.Mine:
				sub = "No longer backed up"
			}
			fmt.Fprintf(out, "  %-24s %s\n", x.Name, sub)
		}
	}
	return nil
}

// findStoredBackup is the one backup a name (and maybe a computer) means.
func findStoredBackup(ctx context.Context, t *trayBackend, name, machine string) (restoreSetOut, error) {
	rows, err := t.Call(ctx, "restoreSets", nil)
	if err != nil {
		return restoreSetOut{}, err
	}
	var match []restoreSetOut
	for _, x := range rows.([]restoreSetOut) {
		if x.Name != name || (machine != "" && x.Machine != machine) {
			continue
		}
		if x.Here {
			return restoreSetOut{}, fmt.Errorf("%q is backed up by this computer. To stop and delete it: r2b remove %s --purge", name, name)
		}
		match = append(match, x)
	}
	switch len(match) {
	case 0:
		return restoreSetOut{}, fmt.Errorf("there is no backup called %q. See them all with: r2b backups", name)
	case 1:
		return match[0], nil
	}
	var machines []string
	for _, x := range match {
		machines = append(machines, x.Machine)
	}
	return restoreSetOut{}, fmt.Errorf("%d computers have a backup called %q (%s). Say which with --machine",
		len(match), name, strings.Join(machines, ", "))
}

// trashSubcommands are the panel's deleted-files actions: recover one,
// delete one forever, empty the trash.
func trashSubcommands(opts *Options) []*cobra.Command {
	var to string
	recover := &cobra.Command{
		Use:   "recover <set> <path>",
		Short: "Bring a deleted file back",
		Long: "Recovers a file from a folder's trash, back where it was or into --to.\n" +
			"A file at that place is replaced, as in the panel.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				res, err := t.d.Restore(cmd.Context(), ui.RestoreRequest{Set: args[0], To: to, Deleted: args[1], Overwrite: true},
					func(string) {}, func(progress.Snapshot) {})
				if err != nil {
					return err
				}
				fmt.Fprintf(opts.Out, "Recovered %s into %s.\n", args[1], res.Target)
				return nil
			})
		},
	}
	recover.Flags().StringVar(&to, "to", "", "recover into this folder instead")

	rm := &cobra.Command{
		Use:   "rm <set> <path>",
		Short: "Delete a file in the trash forever",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				p := newPrompter(opts)
				base := args[1]
				if i := strings.LastIndex(base, "/"); i >= 0 {
					base = base[i+1:]
				}
				if err := confirmDelete(opts, p, "Delete "+base+" forever?", trashWhere(t)); err != nil {
					return err
				}
				n, err := t.trashDelete(cmd.Context(), args[0], args[1], whereBoth)
				if err != nil {
					return err
				}
				if n == 0 {
					return fmt.Errorf("%s is not in %s's trash. See it with: r2b trash ls %s", args[1], args[0], args[0])
				}
				fmt.Fprintf(opts.Out, "Deleted %s.\n", base)
				return nil
			})
		},
	}

	empty := &cobra.Command{
		Use:   "empty [set]",
		Short: "Delete everything in the trash forever",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				set := ""
				if len(args) == 1 {
					set = args[0]
				}
				if err := confirmDelete(opts, newPrompter(opts), "Empty the trash?", trashWhere(t)); err != nil {
					return err
				}
				out, err := t.Call(cmd.Context(), "trashEmpty", mustJSON(map[string]string{"set": set, "where": string(whereBoth)}))
				n := 0
				if m, ok := out.(map[string]int); ok {
					n = m["deleted"]
				}
				if err != nil {
					return err
				}
				fmt.Fprintf(opts.Out, "Trash emptied (%s).\n", countOf(int64(n), "file", "files"))
				return nil
			})
		},
	}
	return []*cobra.Command{recover, rm, empty}
}

// trashWhere is the panel's line under a delete-forever: a trashed file goes
// from the second copy too, which follows where backups go file by file.
func trashWhere(t *trayBackend) string {
	s := "Removed from " + primaryLabel(t)
	if t.d.app.mirror.Exists() {
		s += " and the second copy"
	}
	return s + ". This can’t be undone."
}

func newPackCmd(opts *Options) *cobra.Command {
	var second bool
	cmd := &cobra.Command{
		Use:   "pack on|off",
		Short: "Pack small files into zip packs at a destination, or stop",
		Long: "Packing cuts per-request costs: fewer, larger uploads. It is set for\n" +
			"each destination: where backups go, or with --second-copy the second\n" +
			"copy. Backups already there stay readable either way.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			on := strings.EqualFold(args[0], "on")
			if !on && !strings.EqualFold(args[0], "off") {
				return fmt.Errorf("say on or off, not %q", args[0])
			}
			which := "primary"
			if second {
				which = "second"
			}
			return withBackend(opts, func(t *trayBackend) error {
				store := t.d.app.creds
				if second {
					store = t.d.app.mirror
				}
				if !store.Exists() {
					if second {
						return errors.New("there is no second copy yet. Set one up: r2b mirror setup")
					}
					return errors.New("choose where backups go first: r2b setup")
				}
				out, err := t.Call(cmd.Context(), "setBundle", mustJSON(map[string]any{"which": which, "on": on}))
				if err != nil {
					return err
				}
				st := out.(*storeOut)
				if on {
					fmt.Fprintf(opts.Out, "Packing small files at %s.\n", st.Label)
				} else {
					fmt.Fprintf(opts.Out, "Uploading files one by one at %s.\n", st.Label)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&second, "second-copy", false, "change the second copy, not where backups go")
	return cmd
}

func newUsageCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "usage",
		Short: "What is stored, and what it costs this month",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				if !t.d.app.creds.Exists() {
					return errors.New("choose where backups go first: r2b setup")
				}
				c, err := t.cost(cmd.Context())
				if err != nil {
					return err
				}
				printUsage(opts.Out, c)
				return nil
			})
		},
	}
}

// printUsage is the panel's "Usage and cost" block.
func printUsage(out io.Writer, c costOut) {
	stored := progress.FormatBytes(c.StoredBytes)
	if c.Provider == "drime" {
		fmt.Fprintf(out, "Stored        %s\nFiles         %s\n", stored, progress.FormatCount(c.StoredObjects))
		return
	}
	switch c.BudgetState {
	case "exceeded":
		fmt.Fprintf(out, "Backups paused. Monthly limit of %s reached.\nTo carry on this month: r2b budget continue\n\n", usd(c.BudgetUSD))
	case "near":
		fmt.Fprintf(out, "Close to your monthly limit of %s.\n\n", usd(c.BudgetUSD))
	}
	if c.WithinFreeTier {
		fmt.Fprintln(out, "This month    Free (free tier)")
	} else {
		fmt.Fprintf(out, "This month    %s\nOn track for  %s\n", usd(c.EstimatedUSD), usd(c.ProjectedUSD))
	}
	fmt.Fprintf(out, "Stored        %s, %s\n", stored, countOf(c.StoredObjects, "file", "files"))
	ops := progress.FormatCount(int64(c.OpsUsed))
	if c.OpsLimit > 0 {
		ops += " of " + progress.FormatCount(int64(c.OpsLimit)) + " free"
	}
	fmt.Fprintf(out, "Operations    %s\n", ops)
	limit := "None"
	if c.BudgetUSD > 0 {
		limit = usd(c.BudgetUSD)
	}
	fmt.Fprintf(out, "Monthly limit %s (backups pause here; change it: r2b budget set <amount>)\n", limit)
	fmt.Fprintln(out, "Estimate at Cloudflare’s prices, for r2backup only.")
}

func usd(v float64) string { return "$" + strconv.FormatFloat(v, 'f', 2, 64) }

// newMirrorOffCmd is the panel's "Stop keeping a second copy".
func newMirrorOffCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Stop keeping a second copy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				second, err := t.d.app.mirror.Load()
				if err != nil {
					fmt.Fprintln(opts.Out, "There is no second copy.")
					return nil
				}
				p := newPrompter(opts)
				fmt.Fprintf(p.out, "Stop keeping a second copy?\nWhat’s in %s stays. Nothing more is copied.\n", destLabel(second))
				ok, err := decide(opts, p, "Stop? [y/N]", false)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(opts.Out, "Still keeping a second copy.")
					return nil
				}
				if _, err := t.Call(cmd.Context(), "mirrorForget", nil); err != nil {
					return err
				}
				fmt.Fprintln(opts.Out, "Second copy off.")
				return nil
			})
		},
	}
}

// mustJSON encodes a request the way the panel sends it.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
