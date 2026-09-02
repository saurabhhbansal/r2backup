package schedule

// macOS launchd artifact generation. Pure string generation and parsing
// only -- no build tag, no exec.Command -- so it is exercised on every
// platform's test run, not just macOS's. schedule_darwin.go is the only
// file that actually shells out to launchctl.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// launchdLabel is the launchd Label for e.Name: reverse-DNS-shaped, as
// launchd convention (and its own docs) expect, and safe for use as this
// same string's plist filename.
func launchdLabel(name string) string {
	return "com.r2backup." + sanitizeUnitName(name)
}

// launchdPlist renders the LaunchAgent plist for e. stdoutPath and
// stderrPath point at log files under the data directory: launchd has no
// console to print to for an agent that isn't RunAtLoad-visible, so without
// this a failing scheduled run would leave no trace anywhere.
//
// Arguments are XML-escaped but never shell-quoted: unlike Windows'
// Arguments string or systemd's ExecStart line, ProgramArguments is a plist
// array with one <string> element per argument, so launchd never re-parses
// a joined command line and a space inside a path can never split it into
// two arguments.
func launchdPlist(e Entry, stdoutPath, stderrPath string) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n", xmlEscapeText(launchdLabel(e.Name)))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscapeText(e.BinaryPath))
	for _, a := range e.Args {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscapeText(a))
	}
	b.WriteString("\t</array>\n")
	// StartInterval, not StartCalendarInterval: r2backup needs a fixed
	// cadence, "every N seconds", the same intent as Windows' Repetition
	// and systemd's OnUnitActiveSec.
	fmt.Fprintf(&b, "\t<key>StartInterval</key>\n\t<integer>%d</integer>\n", int64(e.Interval/time.Second))
	// RunAtLoad=true: run when the agent loads, which is at login.
	//
	// This was false, so that loading the agent would not itself trigger a
	// backup. What that actually bought was a macOS machine forgetting: a
	// StartInterval is counted from load, so a run due while the machine was
	// off is simply lost, where Windows catches it up with
	// StartWhenAvailable and systemd with Persistent=true. A backup
	// interrupted by a shutdown then waited a full interval before carrying
	// on, with a half-finished upload sitting on the server the whole time.
	//
	// The cost is honest and small: installing a schedule now runs one
	// backup straight away, and so does every login. A run that finds
	// nothing changed costs no requests on most days, but not literally
	// none: the first run after UTC midnight also does the trash sweep's
	// LIST, one request, and only for a set with retention enabled.
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", xmlEscapeText(stdoutPath))
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", xmlEscapeText(stderrPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.String(), nil
}

var plistStartIntervalRe = regexp.MustCompile(`<key>StartInterval</key>\s*<integer>(\d+)</integer>`)

// launchdParseStartInterval extracts and parses StartInterval from the text
// of an installed plist, for Current.
func launchdParseStartInterval(plist string) (time.Duration, error) {
	m := plistStartIntervalRe.FindStringSubmatch(plist)
	if m == nil {
		return 0, fmt.Errorf("schedule: no StartInterval found in plist")
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("schedule: parse StartInterval: %w", err)
	}
	return time.Duration(secs) * time.Second, nil
}

// parseLaunchctlListStatus reads the single-line "PID\tStatus\tLabel" output
// of `launchctl list <label>` and returns the Status field (the exit code of
// the job's last run, or "-" if it has never run). Best-effort text parsing,
// tolerant of a missing or short line.
func parseLaunchctlListStatus(output string) string {
	line := strings.SplitN(strings.TrimSpace(output), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return fields[1]
	}
	return ""
}
