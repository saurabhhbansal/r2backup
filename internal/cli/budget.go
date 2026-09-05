package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/cost"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/spend"
)

// newBudgetCmd builds `budget`: a monthly ceiling on what backing up costs.
//
// The feature is off until somebody turns it on, and the wording throughout
// leans on "paused" rather than "stopped" because that is the truth of it:
// reaching the limit suspends uploads and `budget continue` lifts the
// suspension for the rest of the month. A limit that could not be lifted
// would be a trap -- the month you hit it is the month you added a lot of
// data, which is exactly when you want it backed up.
func newBudgetCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "budget",
		Short: "Cap what backing up costs each month",
		Long: "Sets a monthly ceiling on the estimated R2 bill. When it is reached,\n" +
			"backups pause -- restores never do -- and `r2b budget continue` starts\n" +
			"them again for the rest of the month.\n\n" +
			"The figures are r2backup's own estimate at Cloudflare's published\n" +
			"rates. Cloudflare exposes no bill this tool can read, so anything else\n" +
			"on the account is not counted here.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return showBudget(cmd, opts)
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "set <amount>",
		Short: "Set the monthly limit, in US dollars",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			amount, err := parseAmount(args[0])
			if err != nil {
				return err
			}
			s, err := config.LoadSettings()
			if err != nil {
				return err
			}
			s.BudgetUSD = amount
			// Setting a limit clears any earlier "carry on". Someone
			// choosing a new ceiling is making a fresh decision, and
			// inheriting a waiver from before it would mean the limit they
			// just set does nothing until next month.
			s.BudgetResumedMonth = ""
			if err := config.SaveSettings(s); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Monthly limit set to $%.2f.\n", amount)
			fmt.Fprintln(opts.Out, "Backups pause when the estimate reaches it. Restores never pause.")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "off",
		Short: "Remove the monthly limit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := config.LoadSettings()
			if err != nil {
				return err
			}
			s.BudgetUSD = 0
			s.BudgetResumedMonth = ""
			if err := config.SaveSettings(s); err != nil {
				return err
			}
			fmt.Fprintln(opts.Out, "Limit removed. Backups will not pause for cost.")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "continue",
		Short: "Carry on backing up for the rest of this month",
		Long: "Lifts a pause caused by the monthly limit, for this calendar month\n" +
			"only. The limit itself is left alone and comes back on the first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := config.LoadSettings()
			if err != nil {
				return err
			}
			if s.BudgetUSD <= 0 {
				fmt.Fprintln(opts.Out, "There is no limit set, so nothing is paused.")
				return nil
			}
			now := time.Now()
			s.BudgetResumedMonth = cost.MonthKey(now)
			if err := config.SaveSettings(s); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Backups will carry on past the $%.2f limit for the rest of %s.\n",
				s.BudgetUSD, now.UTC().Format("January"))
			// Saying when it comes back matters. The value of a limit that
			// expires by itself is that nobody has to remember to restore
			// it, and nobody will trust that unless they are told.
			fmt.Fprintln(opts.Out, "The limit returns on the 1st.")
			return nil
		},
	})

	return cmd
}

// showBudget prints the month so far and where the limit stands.
func showBudget(cmd *cobra.Command, opts *Options) error {
	s, err := config.LoadSettings()
	if err != nil {
		return err
	}
	budget := cost.Budget{LimitUSD: s.BudgetUSD, ResumedMonth: s.BudgetResumedMonth}

	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	idx, release, err := a.checkoutIndex()
	if err != nil {
		return err
	}
	defer release()

	snap, err := spend.Read(idx, budget, time.Now())
	if err != nil {
		return err
	}

	fmt.Fprintf(opts.Out, "Stored      %s in %s objects\n",
		progress.FormatBytes(snap.StoredBytes), progress.FormatCount(snap.Objects))
	fmt.Fprintf(opts.Out, "Operations  %s of %s free this month\n",
		progress.FormatCount(int64(snap.ClassAOps)), progress.FormatCount(int64(snap.OpsLimit)))

	if snap.Cost.WithinFreeTier {
		fmt.Fprintln(opts.Out, "Cost        nothing yet -- still inside the free tier")
	} else {
		fmt.Fprintf(opts.Out, "Cost        about $%.2f so far this month (storage $%.2f, operations $%.2f)\n",
			snap.EstimatedUSD(), snap.Cost.StorageUSD, snap.Cost.ClassAUSD+snap.Cost.ClassBUSD)
		fmt.Fprintf(opts.Out, "            on course for about $%.2f by month end\n", snap.Projected)
	}
	fmt.Fprintln(opts.Out, "            an estimate at Cloudflare's published rates, not a bill")
	fmt.Fprintln(opts.Out)

	switch snap.Verdict {
	case cost.Off:
		fmt.Fprintln(opts.Out, "No monthly limit is set.")
		fmt.Fprintln(opts.Out, "To set one: r2b budget set 5")
	case cost.Within:
		left, _ := budget.Remaining(snap.EstimatedUSD())
		fmt.Fprintf(opts.Out, "Limit       $%.2f a month, $%.2f left.\n", budget.LimitUSD, left)
	case cost.Near:
		left, _ := budget.Remaining(snap.EstimatedUSD())
		fmt.Fprintf(opts.Out, "Limit       $%.2f a month, $%.2f left.\n", budget.LimitUSD, left)
		fmt.Fprintln(opts.Out, "Getting close. Backups pause when it is reached.")
	case cost.Paused:
		fmt.Fprintf(opts.Out, "Backups are paused: the estimate has reached the $%.2f limit.\n", budget.LimitUSD)
		fmt.Fprintln(opts.Out, "Restores still work, and always will.")
		fmt.Fprintln(opts.Out, "To carry on this month: r2b budget continue")
		fmt.Fprintln(opts.Out, "To raise the limit:     r2b budget set <amount>")
	case cost.Resumed:
		fmt.Fprintf(opts.Out, "Past the $%.2f limit, carrying on because you said so.\n", budget.LimitUSD)
		fmt.Fprintln(opts.Out, "The limit comes back on the 1st.")
	}
	return nil
}

// parseAmount reads a dollar figure, forgiving a leading $ and stray spaces
// because someone copying a number off a bill will bring one along.
func parseAmount(raw string) (float64, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "$")
	cleaned = strings.ReplaceAll(cleaned, ",", "")
	amount, err := strconv.ParseFloat(strings.TrimSpace(cleaned), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an amount -- try something like 5 or 12.50", raw)
	}
	if amount < 0 {
		return 0, fmt.Errorf("a limit cannot be negative")
	}
	if amount == 0 {
		// Zero is how the settings file spells "no limit", so accepting it
		// here would make `budget set 0` silently mean `budget off`. Saying
		// so is better than doing something they did not ask for.
		return 0, fmt.Errorf("a limit of zero would pause every backup -- use `r2b budget off` to remove the limit")
	}
	return amount, nil
}
