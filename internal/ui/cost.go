package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/saurabhhbansal/r2backup/internal/progress"
)

// The Cost tab.
//
// Two jobs, and the second is the one that matters. It shows what this is
// costing -- which nothing else in the interface could say, because Cloudflare
// exposes no bill r2backup can read, so these are the tool's own figures at
// published rates. And it is where a paused backup is visible and can be
// started again.
//
// That second job is why this is a tab rather than a line on the Folders
// screen. A spending limit stops backups, and a stopped backup looks exactly
// like a backup with nothing to do; the only thing standing between those two
// readings is somewhere that says so plainly and offers the way out in the
// same breath.

func (m *Model) costView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("What this costs") + "\n\n")

	if !m.ov.Configured {
		b.WriteString(dimStyle.Render("  Nothing yet -- this computer is not set up.") + "\n")
		return b.String()
	}

	b.WriteString(labelStyle.Render("  stored") +
		fmt.Sprintf("%s in %s objects\n",
			progress.FormatBytes(m.ov.StoredBytes),
			progress.FormatCount(m.ov.StoredObjects)))
	b.WriteString(labelStyle.Render("  writes") +
		fmt.Sprintf("%s of %s free this month\n",
			progress.FormatCount(int64(m.ov.OpsUsed)),
			progress.FormatCount(int64(m.ov.OpsLimit))))
	if m.ov.ClassBUsed > 0 {
		b.WriteString(labelStyle.Render("  reads") +
			fmt.Sprintf("%s of %s free this month\n",
				progress.FormatCount(int64(m.ov.ClassBUsed)),
				progress.FormatCount(int64(m.ov.ClassBLimit))))
	}

	b.WriteString("\n")
	if m.ov.WithinFreeTier {
		b.WriteString("  " + goodStyle.Render("Nothing so far this month") +
			dimStyle.Render(" — still inside the free tier.") + "\n")
	} else {
		b.WriteString(labelStyle.Render("  so far") +
			titleStyle.Render(usd(m.ov.EstimatedUSD)) +
			dimStyle.Render(fmt.Sprintf("  (storage %s, operations %s)",
				usd(m.ov.StorageUSD), usd(m.ov.OperationsUSD))) + "\n")
		b.WriteString(labelStyle.Render("  month end") +
			dimStyle.Render("about "+usd(m.ov.ProjectedUSD)+" at this rate") + "\n")
	}
	// Said every time, not once in a help screen. A number that looks like a
	// bill and is not one has to keep saying so.
	b.WriteString("\n" + dimStyle.Render("  An estimate at Cloudflare's published rates, from what r2backup\n"+
		"  itself did. Anything else on your account is not counted here.") + "\n")

	b.WriteString("\n" + m.budgetLines())
	return b.String()
}

// budgetLines renders the limit and, when it has bitten, the way out.
func (m *Model) budgetLines() string {
	var b strings.Builder
	switch m.ov.BudgetState {
	case "paused":
		b.WriteString("  " + badStyle.Render("● Backups are paused") +
			dimStyle.Render(" — the estimate reached your "+usd(m.ov.BudgetUSD)+" limit.") + "\n")
		// Restores never pause, and someone reading a screen that says
		// "paused" needs to know that before they panic about their data.
		b.WriteString(dimStyle.Render("  Restoring still works, and always will.") + "\n\n")
		b.WriteString("  " + titleStyle.Render("c") + dimStyle.Render(" carry on this month") +
			dimStyle.Render("    ") + titleStyle.Render("b") + dimStyle.Render(" raise the limit") +
			dimStyle.Render("    ") + titleStyle.Render("x") + dimStyle.Render(" remove it"))
	case "resumed":
		b.WriteString("  " + warnStyle.Render("● Past your "+usd(m.ov.BudgetUSD)+" limit") +
			dimStyle.Render(" — carrying on because you said so.") + "\n")
		b.WriteString(dimStyle.Render("  The limit comes back on the 1st.") + "\n\n")
		b.WriteString("  " + titleStyle.Render("b") + dimStyle.Render(" change the limit") +
			dimStyle.Render("    ") + titleStyle.Render("x") + dimStyle.Render(" remove it"))
	case "near":
		b.WriteString("  " + warnStyle.Render("● Close to your "+usd(m.ov.BudgetUSD)+" limit") +
			dimStyle.Render(" — backups pause when it is reached.") + "\n\n")
		b.WriteString("  " + titleStyle.Render("b") + dimStyle.Render(" change the limit") +
			dimStyle.Render("    ") + titleStyle.Render("x") + dimStyle.Render(" remove it"))
	case "within":
		left := m.ov.BudgetUSD - m.ov.EstimatedUSD
		if left < 0 {
			left = 0
		}
		b.WriteString("  " + goodStyle.Render("● Limit "+usd(m.ov.BudgetUSD)+" a month") +
			dimStyle.Render(" — "+usd(left)+" left.") + "\n\n")
		b.WriteString("  " + titleStyle.Render("b") + dimStyle.Render(" change the limit") +
			dimStyle.Render("    ") + titleStyle.Render("x") + dimStyle.Render(" remove it"))
	default: // "off", and anything a future build has not taught this one
		b.WriteString("  " + dimStyle.Render("○ No monthly limit.") + "\n\n")
		b.WriteString("  " + titleStyle.Render("b") + dimStyle.Render(" set a monthly limit"))
	}
	return b.String()
}

func (m *Model) costKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Limit):
		m.askBudget()
		return m, nil

	case key.Matches(msg, keys.Continue):
		// Only offered when something is actually paused. Elsewhere the key
		// does nothing rather than quietly pre-authorising an overspend
		// nobody has been warned about yet.
		if m.ov.BudgetState != "paused" {
			return m, nil
		}
		return m, m.resumeBudget()

	case key.Matches(msg, keys.NoLimit):
		if m.ov.BudgetUSD <= 0 {
			return m, nil
		}
		return m, m.setBudget(0)
	}
	return m, nil
}

// askBudget opens the form for the monthly limit.
func (m *Model) askBudget() {
	title := "Set a monthly limit"
	if m.ov.BudgetUSD > 0 {
		title = "Change the monthly limit"
	}
	m.showForm(newForm(
		title,
		"In US dollars. Backups pause when the estimate reaches it; restores never pause.",
		[]field{{Label: "Limit, e.g. 5"}},
		func(vals []string) (string, tea.Cmd) {
			amount, err := parseUSD(vals[0])
			if err != nil {
				return err.Error(), nil
			}
			return "", m.setBudget(amount)
		}))
}

func (m *Model) setBudget(amount float64) tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.SetBudget(m.ctx, amount); err != nil {
			return errMsg{err}
		}
		if amount <= 0 {
			return noticeMsg("Limit removed. Backups will not pause for cost.")
		}
		return noticeMsg(fmt.Sprintf("Limit set to %s a month.", usd(amount)))
	}
}

func (m *Model) resumeBudget() tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.ResumeBudget(m.ctx); err != nil {
			return errMsg{err}
		}
		return noticeMsg("Backups will carry on for the rest of the month. The limit returns on the 1st.")
	}
}

// parseUSD reads a dollar figure from a form field, forgiving the currency
// symbol and thousands separators someone copying a number will bring along.
func parseUSD(raw string) (float64, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "$")
	cleaned = strings.ReplaceAll(cleaned, ",", "")
	amount, err := strconv.ParseFloat(strings.TrimSpace(cleaned), 64)
	if err != nil {
		return 0, fmt.Errorf("That is not an amount. Try something like 5 or 12.50.")
	}
	if amount < 0 {
		return 0, fmt.Errorf("A limit cannot be negative.")
	}
	if amount == 0 {
		// Zero is how "no limit" is stored, so accepting it here would make
		// a typed 0 silently mean the opposite of what it looks like.
		return 0, fmt.Errorf("A limit of zero would pause every backup. Press x to remove the limit instead.")
	}
	return amount, nil
}

// usd formats money for the screen. Two decimal places always, because a
// figure that renders as "$5" next to one that renders as "$4.50" reads as a
// different kind of number rather than a rounder one.
func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }
