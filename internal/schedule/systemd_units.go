package schedule

// systemd user unit artifact generation for the Linux implementation. Pure
// string generation and parsing only -- no build tag, no exec.Command --
// so it is exercised on every platform's test run, not just Linux's.
// schedule_linux.go is the only file that actually shells out to systemctl.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// systemdDuration renders d in systemd's time-span grammar (systemd.time(7)),
// preferring a human unit over a bare integer: "30min" or "2h" tells anyone
// reading `systemctl cat` what the timer does without doing arithmetic. It
// only steps down to a smaller unit when d isn't a whole number of the
// larger one, so precision is never silently lost.
func systemdDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dmin", int64(d/time.Minute))
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	default:
		return fmt.Sprintf("%dus", d.Microseconds())
	}
}

var systemdDurationRe = regexp.MustCompile(`^(\d+)(h|min|s|us)$`)

// parseSystemdDuration reverses systemdDuration, for reading OnUnitActiveSec
// back out of an installed .timer file in Current.
func parseSystemdDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	m := systemdDurationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("schedule: %q is not a duration this package emits", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("schedule: parse duration %q: %w", s, err)
	}
	switch m[2] {
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "min":
		return time.Duration(n) * time.Minute, nil
	case "s":
		return time.Duration(n) * time.Second, nil
	case "us":
		return time.Duration(n) * time.Microsecond, nil
	}
	return 0, fmt.Errorf("schedule: unrecognized duration unit in %q", s)
}

// systemdQuoteArg quotes a single ExecStart argument per systemd.service(5)
// "Command Lines" rules: wrap it in double quotes and backslash-escape any
// embedded backslash or double quote. Needed for a path containing a space,
// e.g. `/home/my user/bin/r2backup`, which without quoting systemd would
// split into two arguments.
func systemdQuoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"'$`\\") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// systemdExecStart quotes and joins BinaryPath and args into one ExecStart=
// value.
func systemdExecStart(binaryPath string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, systemdQuoteArg(binaryPath))
	for _, a := range args {
		parts = append(parts, systemdQuoteArg(a))
	}
	return strings.Join(parts, " ")
}

// systemdTimerUnit renders the <name>.timer unit for e.
func systemdTimerUnit(e Entry) string {
	return fmt.Sprintf(`[Unit]
Description=Runs %s every %s

[Timer]
# OnUnitActiveSec, not OnCalendar: r2backup needs "every N minutes since it
# last ran", a fixed repetition, not a wall-clock schedule.
OnUnitActiveSec=%s
# OnActiveSec is relative to the timer unit's own activation, unlike
# OnUnitActiveSec which is relative to the service's last activation and so
# never elapses until the service has run once. This gives the timer an
# initial trigger -- a minute after "systemctl --user start" at install, and
# a minute after the user manager comes up at each login.
OnActiveSec=1min
Unit=%s.service

[Install]
WantedBy=timers.target
`, e.Name, e.Interval, systemdDuration(e.Interval), systemdUnitName(e.Name))
}

// systemdServiceUnit renders the <name>.service unit for e.
func systemdServiceUnit(e Entry) string {
	return fmt.Sprintf(`[Unit]
Description=%s (r2backup scheduled run)

[Service]
# Type=oneshot: this process runs, exits, and is done. There is nothing for
# systemd to keep resident -- r2backup ships no daemon on any platform.
Type=oneshot
ExecStart=%s
`, e.Name, systemdExecStart(e.BinaryPath, e.Args))
}

// systemdUnitName is the basename (without extension) shared by the
// .service and .timer files, and the argument passed to systemctl.
func systemdUnitName(name string) string {
	return sanitizeUnitName(name)
}

var (
	onUnitActiveSecRe = regexp.MustCompile(`(?m)^OnUnitActiveSec=(.+)$`)
)

// parseSystemdTimerInterval extracts and parses OnUnitActiveSec from the
// text of an installed .timer file.
func parseSystemdTimerInterval(unitFile string) (time.Duration, error) {
	m := onUnitActiveSecRe.FindStringSubmatch(unitFile)
	if m == nil {
		return 0, fmt.Errorf("schedule: no OnUnitActiveSec found in timer unit")
	}
	return parseSystemdDuration(strings.TrimSpace(m[1]))
}

// parseSystemdShow parses the "Key=Value\n" lines `systemctl show` prints
// into a map. Pure text parsing so Current's use of it is testable without a
// running systemd instance.
func parseSystemdShow(output string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		props[line[:idx]] = line[idx+1:]
	}
	return props
}

// systemdTimestampLayouts are the formats `systemctl show` is known to print
// for a *USec* timestamp property. Best-effort: a value in a layout not
// listed here parses to the zero time rather than an error, since the exact
// layout varies by systemd version and locale and NextRun/LastRun are
// documented as best-effort in Status.
var systemdTimestampLayouts = []string{
	"Mon 2006-01-02 15:04:05 MST",
	"Mon 2006-01-02 15:04:05 -0700",
}

func parseSystemdTimestamp(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" || v == "n/a" || v == "0" {
		return time.Time{}
	}
	for _, layout := range systemdTimestampLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
