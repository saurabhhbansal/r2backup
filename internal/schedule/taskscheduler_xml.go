package schedule

// Windows Task Scheduler artifact generation: the XML definition schtasks
// consumes, and the small amount of parsing needed to read one back. None of
// this touches the OS -- it is plain string and byte manipulation, so a
// Linux (or macOS) test run verifies the exact same code a Windows machine
// would use. Only schedule_windows.go, which actually shells out to
// schtasks, is build-tagged to Windows.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// windowsTaskNS is the XML namespace every Task Scheduler 1.2 definition
// must declare; schtasks /create /xml rejects a document without it.
const windowsTaskNS = "http://schemas.microsoft.com/windows/2004/02/mit/task"

// windowsISO8601Duration renders d as the ISO 8601 duration Task Scheduler's
// Repetition/Interval element expects, e.g. 30m -> "PT30M", 90m -> "PT1H30M".
func windowsISO8601Duration(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	total := int64(d / time.Second)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	var b strings.Builder
	b.WriteString("PT")
	if h > 0 {
		fmt.Fprintf(&b, "%dH", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dM", m)
	}
	if s > 0 || (h == 0 && m == 0) {
		fmt.Fprintf(&b, "%dS", s)
	}
	return b.String()
}

var iso8601DurationRe = regexp.MustCompile(`^P(?:\d+D)?T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?$`)

// parseISO8601Duration reverses windowsISO8601Duration, for reading a
// Repetition/Interval back out of a task definition in Current.
func parseISO8601Duration(s string) (time.Duration, error) {
	m := iso8601DurationRe.FindStringSubmatch(s)
	if m == nil || (m[1] == "" && m[2] == "" && m[3] == "") {
		return 0, fmt.Errorf("schedule: %q is not an ISO 8601 duration this package emits", s)
	}
	var d time.Duration
	if m[1] != "" {
		h, _ := strconv.Atoi(m[1])
		d += time.Duration(h) * time.Hour
	}
	if m[2] != "" {
		mm, _ := strconv.Atoi(m[2])
		d += time.Duration(mm) * time.Minute
	}
	if m[3] != "" {
		s2, _ := strconv.Atoi(m[3])
		d += time.Duration(s2) * time.Second
	}
	return d, nil
}

// windowsQuoteArg quotes a single command-line argument the way Windows'
// CommandLineToArgvW expects: wrap in quotes if it contains whitespace or a
// quote, doubling any backslashes that immediately precede a quote and
// backslash-escaping the quote itself. Without this, a path like
// `C:\Program Files\r2backup\r2backup.exe` splits into two arguments at the
// space and Task Scheduler tries to run `C:\Program`.
func windowsQuoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
			b.WriteRune(r)
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			slashes = 0
			b.WriteRune(r)
		}
	}
	for ; slashes > 0; slashes-- {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

// windowsCommandLine quotes and joins Args for the Exec/Arguments element.
func windowsCommandLine(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = windowsQuoteArg(a)
	}
	return strings.Join(parts, " ")
}

// windowsTaskXML builds the Task Scheduler 1.2 task definition for e. userID
// is the Principal's UserId (typically DOMAIN\user or just the local
// username); it is a parameter rather than resolved internally via
// os/user.Current so this function stays a pure, deterministic function of
// its inputs and can be tested without a real Windows account.
//
// Every Settings value below is load-bearing -- see the table in the
// package's task description for the failure each one prevents.
// Logon types the task XML can be built with. S4U runs whether or not the
// user is signed in and stores no password, which is what a backup wants --
// but registering an S4U task needs the "Log on as a batch job" right, and
// schtasks refuses without it. InteractiveToken asks for no privilege at all
// and runs whenever the user is signed in, which is the honest second best.
const (
	logonS4U              = "S4U"
	logonInteractiveToken = "InteractiveToken"
)

func windowsTaskXML(e Entry, userID string) (string, error) {
	return windowsTaskXMLAs(e, userID, logonS4U)
}

func windowsTaskXMLAs(e Entry, userID, logonType string) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	interval := windowsISO8601Duration(e.Interval)
	command := xmlEscapeText(windowsQuoteArg(e.BinaryPath))
	arguments := xmlEscapeText(windowsCommandLine(e.Args))
	user := xmlEscapeText(userID)
	desc := xmlEscapeText(fmt.Sprintf(
		"r2backup scheduled run %q, every %s. Installed by r2backup; edits made here are overwritten on the next install.",
		e.Name, e.Interval))

	var b strings.Builder
	// Declared as UTF-16 because the file on disk is UTF-16 with a BOM --
	// see utf16BOMBytes below. schtasks /create /xml rejects a UTF-8 file
	// outright, so the declaration and the bytes must agree.
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n")
	fmt.Fprintf(&b, "<Task version=\"1.2\" xmlns=\"%s\">\n", windowsTaskNS)
	b.WriteString("  <RegistrationInfo>\n")
	fmt.Fprintf(&b, "    <Description>%s</Description>\n", desc)
	b.WriteString("  </RegistrationInfo>\n")
	b.WriteString("  <Triggers>\n")
	b.WriteString("    <CalendarTrigger>\n")
	// A fixed StartBoundary in the past plus a daily ScheduleByDay is just
	// the anchor Task Scheduler needs before it will accept a Repetition --
	// the actual cadence comes entirely from Repetition/Interval below.
	b.WriteString("      <StartBoundary>2020-01-01T00:00:00</StartBoundary>\n")
	b.WriteString("      <Enabled>true</Enabled>\n")
	b.WriteString("      <ScheduleByDay>\n        <DaysInterval>1</DaysInterval>\n      </ScheduleByDay>\n")
	b.WriteString("      <Repetition>\n")
	// The interval that actually matters: how often r2backup runs.
	fmt.Fprintf(&b, "        <Interval>%s</Interval>\n", interval)
	// Duration=P1D + StopAtDurationEnd=false: the repetition re-anchors
	// every day and keeps firing forever, instead of stopping after one day.
	b.WriteString("        <Duration>P1D</Duration>\n")
	b.WriteString("        <StopAtDurationEnd>false</StopAtDurationEnd>\n")
	b.WriteString("      </Repetition>\n")
	b.WriteString("    </CalendarTrigger>\n")
	b.WriteString("  </Triggers>\n")
	b.WriteString("  <Principals>\n")
	b.WriteString("    <Principal id=\"Author\">\n")
	fmt.Fprintf(&b, "      <UserId>%s</UserId>\n", user)
	// See logonS4U / logonInteractiveToken. Either way no password is
	// stored. Note that neither of them, and nothing else in this document,
	// keeps a console window off the screen -- see Hidden below.
	fmt.Fprintf(&b, "      <LogonType>%s</LogonType>\n", logonType)
	b.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	b.WriteString("    </Principal>\n")
	b.WriteString("  </Principals>\n")
	b.WriteString("  <Settings>\n")
	// MultipleInstances=IgnoreNew: a long run is never stacked on by the
	// next tick -- the new attempt is skipped instead of piling up.
	b.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	// Both false: a laptop must still get backed up. These default to true
	// in the Task Scheduler UI, which would silently skip every run on a
	// machine that is ever unplugged.
	b.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	b.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	b.WriteString("    <AllowHardTerminate>true</AllowHardTerminate>\n")
	// StartWhenAvailable=true: catches up after the machine was asleep or
	// switched off through a tick, instead of waiting for the next one.
	b.WriteString("    <StartWhenAvailable>true</StartWhenAvailable>\n")
	b.WriteString("    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>\n")
	b.WriteString("    <Enabled>true</Enabled>\n")
	// Hidden=true hides the task from Task Scheduler's own list view. It is
	// NOT what keeps a console window off the screen, however much it reads
	// like it: Task Scheduler cannot launch a process without a window at
	// all. A console-subsystem binary gets its console from the loader, and
	// under the InteractiveToken fallback that console is on the user's
	// desktop for the whole length of the backup. The process closes its own
	// window instead -- see internal/winconsole, and the --hidden argument
	// the CLI puts on the command line for exactly that.
	b.WriteString("    <Hidden>true</Hidden>\n")
	// ExecutionTimeLimit=PT0S means "no limit" to Task Scheduler: a
	// six-hour first backup is not killed partway through the way a
	// nonzero default eventually would kill it on a very large dataset.
	b.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n")
	b.WriteString("    <Priority>7</Priority>\n")
	b.WriteString("  </Settings>\n")
	b.WriteString("  <Actions Context=\"Author\">\n")
	b.WriteString("    <Exec>\n")
	fmt.Fprintf(&b, "      <Command>%s</Command>\n", command)
	if arguments != "" {
		fmt.Fprintf(&b, "      <Arguments>%s</Arguments>\n", arguments)
	}
	b.WriteString("    </Exec>\n")
	b.WriteString("  </Actions>\n")
	b.WriteString("</Task>\n")
	return b.String(), nil
}

// utf16BOMBytes encodes s as UTF-16LE with a leading byte-order mark.
// schtasks /create /xml refuses a UTF-8 file, so this is the step between
// windowsTaskXML's plain Go string and the bytes actually written to disk.
func utf16BOMBytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 2+2*len(units))
	buf[0], buf[1] = 0xFF, 0xFE // little-endian BOM
	for i, u := range units {
		buf[2+2*i] = byte(u)
		buf[2+2*i+1] = byte(u >> 8)
	}
	return buf
}

// decodeUTF16BOM reverses utf16BOMBytes, for tests and for parsing schtasks
// output that turns out to carry the same BOM.
func decodeUTF16BOM(b []byte) (string, error) {
	if len(b) < 2 {
		return "", fmt.Errorf("schedule: %d bytes is too short for a UTF-16 BOM", len(b))
	}
	var little bool
	switch {
	case b[0] == 0xFF && b[1] == 0xFE:
		little = true
	case b[0] == 0xFE && b[1] == 0xFF:
		little = false
	default:
		return "", fmt.Errorf("schedule: no UTF-16 BOM at start of input")
	}
	body := b[2:]
	if len(body)%2 != 0 {
		return "", fmt.Errorf("schedule: odd number of bytes after BOM")
	}
	units := make([]uint16, len(body)/2)
	for i := range units {
		if little {
			units[i] = uint16(body[2*i]) | uint16(body[2*i+1])<<8
		} else {
			units[i] = uint16(body[2*i])<<8 | uint16(body[2*i+1])
		}
	}
	return string(utf16.Decode(units)), nil
}

var repetitionIntervalRe = regexp.MustCompile(`<Interval>([^<]+)</Interval>`)

// logonTypeRe pulls the Principal's LogonType back out of a task definition,
// so the CLI can say which of the two Install actually got rather than
// assuming the one it asked for first.
// The character class has to allow digits: the value this exists to detect is
// literally "S4U", and [A-Za-z]+ silently matched nothing at all.
var logonTypeRe = regexp.MustCompile(`<LogonType>\s*([A-Za-z0-9]+)\s*</LogonType>`)

// windowsParseTaskLogonType returns the LogonType of a task definition as
// printed by `schtasks /query /xml`, or "" when there is none to read.
func windowsParseTaskLogonType(xmlOutput string) string {
	if m := logonTypeRe.FindStringSubmatch(xmlOutput); m != nil {
		return m[1]
	}
	return ""
}

// windowsParseTaskInterval extracts and parses the Repetition/Interval
// element from a task definition, as printed by `schtasks /query /xml`.
func windowsParseTaskInterval(xmlOutput string) (time.Duration, error) {
	m := repetitionIntervalRe.FindStringSubmatch(xmlOutput)
	if m == nil {
		return 0, fmt.Errorf("schedule: no <Interval> element found in task XML")
	}
	return parseISO8601Duration(m[1])
}

// parseSchtasksListOutput reads the "Key:    Value" text schtasks prints for
// `/query /fo LIST /v`. Best-effort: a field it can't find or can't parse is
// left at its zero value rather than treated as an error, since the exact
// key names and time format schtasks prints vary by Windows locale.
func parseSchtasksListOutput(output string) Status {
	st := Status{Registered: true}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "Next Run Time":
			if t, err := time.Parse("1/2/2006 3:04:05 PM", val); err == nil {
				st.NextRun = t
			}
		case "Last Run Time":
			if t, err := time.Parse("1/2/2006 3:04:05 PM", val); err == nil {
				st.LastRun = t
			}
		case "Last Result":
			st.LastResult = val
		}
	}
	return st
}

// isTaskNotFound recognizes schtasks' "no such task" outcome so Remove and
// Current can treat it as "not registered" rather than an error.
func isTaskNotFound(output string) bool {
	low := strings.ToLower(output)
	return strings.Contains(low, "cannot find") || strings.Contains(low, "does not exist") ||
		strings.Contains(low, "no instance") || strings.Contains(low, "not found")
}
