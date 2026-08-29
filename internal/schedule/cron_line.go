package schedule

// crontab fallback artifact generation, used on Linux when systemd's user
// instance is not available (containers, some distros). Pure string
// generation only -- no build tag, no exec.Command -- so it is exercised on
// every platform's test run. schedule_linux.go is the only file that
// actually shells out to crontab.

import (
	"fmt"
	"strings"
	"time"
)

// cronMarker tags every crontab line this package writes, appended as a
// trailing shell comment. It lets Install find and replace its own prior
// entries without disturbing anything else in the user's crontab, and lets
// Remove delete only what it added. A trailing "# ..." works here because
// cron hands the rest of the line to `sh -c`, and sh itself treats a
// whitespace-preceded "#" as a comment to end of line -- so the marker never
// reaches the actual command.
func cronMarker(name string) string {
	return fmt.Sprintf("# r2backup:%s", name)
}

// cronQuoteArg quotes a single word for a POSIX sh command line embedded in
// a crontab entry, using the standard single-quote escape (quote-backslash-quote-quote). Needed for
// a path containing a space, e.g. `/home/my user/bin/r2backup`, which
// without quoting would split into two words when cron hands the line to sh.
func cronQuoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`%") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cronCommand quotes and joins binaryPath and args into the command portion
// of a crontab line.
func cronCommand(binaryPath string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, cronQuoteArg(binaryPath))
	for _, a := range args {
		parts = append(parts, cronQuoteArg(a))
	}
	return strings.Join(parts, " ")
}

// cronSchedule renders interval as one or more 5-field cron time
// specifications whose union fires every `interval`. Cron has no native
// "every N minutes" for an N that doesn't evenly divide 60 (unlike systemd's
// OnUnitActiveSec or Task Scheduler's Repetition, which take an arbitrary
// duration), so an interval that divides a day evenly but not an hour --
// like 90 minutes -- is expanded into an explicit list of minute:hour pairs.
// An interval that doesn't even divide a day evenly is rejected outright
// rather than approximated, since any fixed daily crontab for it would drift
// a little more with every day that passes.
func cronSchedule(interval time.Duration) ([]string, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("schedule: cron interval must be positive, got %s", interval)
	}
	if interval%time.Minute != 0 {
		return nil, fmt.Errorf("schedule: cron interval %s is not a whole number of minutes, cron cannot express it", interval)
	}
	totalMinutes := int64(interval / time.Minute)
	switch {
	case totalMinutes <= 60 && 60%totalMinutes == 0:
		return []string{fmt.Sprintf("*/%d * * * *", totalMinutes)}, nil
	case totalMinutes%60 == 0 && 24%(totalMinutes/60) == 0:
		return []string{fmt.Sprintf("0 */%d * * *", totalMinutes/60)}, nil
	case (24*60)%totalMinutes == 0:
		return cronExplicitSchedule(totalMinutes), nil
	default:
		return nil, fmt.Errorf("schedule: %s does not evenly divide a day, cron cannot express it without drifting", interval)
	}
}

// cronExplicitSchedule handles an interval that divides a day evenly but not
// an hour (e.g. 90 minutes, which lands at :00 and :30 on alternating
// hours): it lists every minute:hour the job fires at across 24h, then
// groups those by minute-of-hour into one cron line per distinct minute
// value, so the result is still just a handful of lines rather than one per
// occurrence.
func cronExplicitSchedule(totalMinutes int64) []string {
	hoursByMinute := map[int64][]int64{}
	var minuteOrder []int64
	for t := int64(0); t < 24*60; t += totalMinutes {
		minute := t % 60
		hour := t / 60
		if _, ok := hoursByMinute[minute]; !ok {
			minuteOrder = append(minuteOrder, minute)
		}
		hoursByMinute[minute] = append(hoursByMinute[minute], hour)
	}
	lines := make([]string, 0, len(minuteOrder))
	for _, minute := range minuteOrder {
		hours := hoursByMinute[minute]
		hourStrs := make([]string, len(hours))
		for i, h := range hours {
			hourStrs[i] = fmt.Sprintf("%d", h)
		}
		lines = append(lines, fmt.Sprintf("%d %s * * *", minute, strings.Join(hourStrs, ",")))
	}
	return lines
}

// cronEntries renders the full crontab lines -- schedule, command, and
// trailing marker -- for e. Each line is independently identifiable via
// cronMarker so Install can replace a prior registration and Remove can
// delete it.
func cronEntries(e Entry) ([]string, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	schedules, err := cronSchedule(e.Interval)
	if err != nil {
		return nil, err
	}
	cmd := cronCommand(e.BinaryPath, e.Args)
	marker := cronMarker(e.Name)
	lines := make([]string, len(schedules))
	for i, sched := range schedules {
		lines[i] = fmt.Sprintf("%s %s %s", sched, cmd, marker)
	}
	return lines, nil
}
