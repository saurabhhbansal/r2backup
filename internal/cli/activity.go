package cli

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/tray"
)

// newActivityCmd is the panel's Activity tab: recent runs, and one run's
// own page.
func newActivityCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "activity [number]",
		Short: "Recent backups, and what one of them did",
		Long: "Lists the latest backups, newest first. Give a run's number for what\n" +
			"it did: uploaded, moved or renamed, moved to deleted files, problems.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := historyPath()
			if err != nil {
				return err
			}
			hist, err := runstate.ReadHistory(p)
			if err != nil {
				return err
			}
			runs := tray.RecentRuns(hist.Runs, time.Now())
			if len(args) == 1 {
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 1 || n > len(runs) {
					return fmt.Errorf("there is no run %s. `r2b activity` lists them", args[0])
				}
				printRun(opts.Out, runs[n-1])
				return nil
			}
			printActivity(opts.Out, runs)
			return nil
		},
	}
}

func printActivity(out io.Writer, runs []tray.Recent) {
	if len(runs) == 0 {
		fmt.Fprintln(out, "No backups yet.")
		return
	}
	day := ""
	for i, r := range runs {
		if r.Day != day {
			if day != "" {
				fmt.Fprintln(out)
			}
			day = r.Day
			fmt.Fprintln(out, day)
		}
		fmt.Fprintf(out, "  %2d. %s  %s  %s\n", i+1, r.Time, r.Set, r.Text)
	}
	fmt.Fprintln(out, "\nFor what one did: r2b activity <number>")
}

func printRun(out io.Writer, r tray.Recent) {
	fmt.Fprintf(out, "%s: %s\n", r.Set, tray.RunTitle(r))
	facts := r.Day + " at " + r.Time
	if r.Took != "" {
		facts += " · took " + r.Took
	}
	fmt.Fprintln(out, facts)
	if r.Error != "" {
		fmt.Fprintf(out, "\n%s\n", r.Error)
	}
	n := func(k int) string { return countOf(int64(k), "file", "files") }
	rows := [][2]string{}
	if r.Uploaded > 0 {
		v := n(r.Uploaded)
		if r.Bytes != "" {
			v += " · " + r.Bytes
		}
		rows = append(rows, [2]string{"Uploaded", v})
	}
	for _, x := range []struct {
		k     int
		label string
	}{
		{r.Moved, "Moved or renamed"}, {r.Deleted, "Moved to deleted files"}, {r.Unchanged, "Already backed up"},
		{r.Failures, "Could not be uploaded"}, {r.Problems, "Could not be read"}, {r.Collisions, "Names that clash"},
	} {
		if x.k > 0 {
			rows = append(rows, [2]string{x.label, n(x.k)})
		}
	}
	if len(rows) > 0 {
		fmt.Fprintln(out)
		for _, row := range rows {
			fmt.Fprintf(out, "  %-24s %s\n", row[0], row[1])
		}
	}
	if len(r.Examples) > 0 {
		fmt.Fprintln(out, "\nFor example")
		for _, x := range r.Examples {
			fmt.Fprintf(out, "  %s\n", x)
		}
	}
	if r.Failures > 0 || r.Problems > 0 {
		fmt.Fprintln(out, "\nThe next backup tries these again.")
	}
}

// dayLabel names the day t falls on as the panel does: "Today",
// "Yesterday", or "Mon 26 Oct".
func dayLabel(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	same := func(a, b time.Time) bool {
		y1, m1, d1 := a.Date()
		y2, m2, d2 := b.Date()
		return y1 == y2 && m1 == m2 && d1 == d2
	}
	switch {
	case same(t, now):
		return "Today"
	case same(t, now.AddDate(0, 0, -1)):
		return "Yesterday"
	}
	return t.Format("Mon 2 Jan")
}
