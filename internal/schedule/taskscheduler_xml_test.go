package schedule

// Windows Task Scheduler XML generator tests. These call plain functions
// with no build tag, so they run -- and must pass -- on every platform's
// `go test`, not only on a Windows runner.

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

func windowsEntry() Entry {
	return Entry{
		Name:       "r2backup-default",
		Interval:   30 * time.Minute,
		BinaryPath: `C:\Program Files\r2backup\r2backup.exe`,
		Args:       []string{"run", "--all"},
	}
}

// TestWindowsTaskXMLSettings asserts on each load-bearing Settings value
// individually and names the consequence of getting it wrong, per the
// package's design brief.
func TestWindowsTaskXMLSettings(t *testing.T) {
	doc, err := windowsTaskXML(windowsEntry(), `DOMAIN\user`)
	if err != nil {
		t.Fatalf("windowsTaskXML: %v", err)
	}
	cases := []struct {
		must string
		why  string
	}{
		{"<LogonType>S4U</LogonType>", "without S4U the task either stores the user's password or needs an interactive session, and a console window flashes on screen every 30 minutes"},
		{"<Hidden>true</Hidden>", "without Hidden nothing stops a window appearing while it works"},
		{"<StartWhenAvailable>true</StartWhenAvailable>", "without StartWhenAvailable a run missed while asleep or off is skipped instead of caught up"},
		{"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "without IgnoreNew a long run gets a second copy stacked on top of it at the next tick"},
		{"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "without PT0S a six-hour first backup is killed partway through by the default time limit"},
		{"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>", "left at the Task Scheduler UI default of true, a laptop on battery never gets backed up"},
		{"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>", "left at the Task Scheduler UI default of true, unplugging mid-backup kills the run"},
	}
	for _, tc := range cases {
		if !strings.Contains(doc, tc.must) {
			t.Errorf("task XML missing %q: %s\n--- XML ---\n%s", tc.must, tc.why, doc)
		}
	}
}

// mustParseXML checks a document is well-formed.
//
// The document declares encoding="UTF-16" because that is what Task Scheduler
// requires of the file on disk. The string in hand is already Go's native
// UTF-8, so the declaration describes the file we will write, not this value --
// hence a CharsetReader that passes the reader straight through. Without one,
// Go's decoder refuses any non-UTF-8 declaration outright.
func mustParseXML(t *testing.T, doc string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("task XML does not parse as XML: %v\n--- XML ---\n%s", err, doc)
		}
	}
}

func TestWindowsTaskXMLIsWellFormedXML(t *testing.T) {
	doc, err := windowsTaskXML(windowsEntry(), `DOMAIN\user`)
	if err != nil {
		t.Fatalf("windowsTaskXML: %v", err)
	}
	mustParseXML(t, doc)
}

func TestWindowsTaskXMLRoundTripsThroughUTF16BOM(t *testing.T) {
	doc, err := windowsTaskXML(windowsEntry(), `DOMAIN\user`)
	if err != nil {
		t.Fatalf("windowsTaskXML: %v", err)
	}
	encoded := utf16BOMBytes(doc)

	if len(encoded) < 2 || encoded[0] != 0xFF || encoded[1] != 0xFE {
		t.Fatalf("utf16BOMBytes did not start with a little-endian BOM (FF FE): got %x", encoded[:min(4, len(encoded))])
	}

	decoded, err := decodeUTF16BOM(encoded)
	if err != nil {
		t.Fatalf("decodeUTF16BOM: %v", err)
	}
	if decoded != doc {
		t.Error("decodeUTF16BOM(utf16BOMBytes(doc)) != doc: encoding is not a clean round trip")
	}

	mustParseXML(t, decoded)
}

func TestWindowsISO8601Duration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Minute, "PT30M"},
		{2 * time.Hour, "PT2H"},
		{90 * time.Minute, "PT1H30M"},
	}
	for _, tc := range cases {
		if got := windowsISO8601Duration(tc.in); got != tc.want {
			t.Errorf("windowsISO8601Duration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseISO8601DurationRoundTrips(t *testing.T) {
	for _, d := range []time.Duration{30 * time.Minute, 2 * time.Hour, 90 * time.Minute} {
		s := windowsISO8601Duration(d)
		got, err := parseISO8601Duration(s)
		if err != nil {
			t.Fatalf("parseISO8601Duration(%q): %v", s, err)
		}
		if got != d {
			t.Errorf("parseISO8601Duration(windowsISO8601Duration(%s)) = %s, want %s", d, got, d)
		}
	}
}

func TestWindowsQuoteArgSpacesAndQuotes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"run", "run"},
		{"a user", `"a user"`},
		{`C:\Program Files\r2backup\r2backup.exe`, `"C:\Program Files\r2backup\r2backup.exe"`},
		{"", `""`},
	}
	for _, tc := range cases {
		if got := windowsQuoteArg(tc.in); got != tc.want {
			t.Errorf("windowsQuoteArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWindowsTaskXMLRejectsInvalidEntry(t *testing.T) {
	if _, err := windowsTaskXML(Entry{}, "user"); err == nil {
		t.Error("windowsTaskXML(Entry{}, ...) = nil error, want validation to reject an empty Entry")
	}
}

func TestParseSchtasksListOutput(t *testing.T) {
	const sample = "Folder:                               \\\r\n" +
		"HostName:                             MYPC\r\n" +
		"TaskName:                             \\r2backup-default\r\n" +
		"Next Run Time:                        8/28/2026 9:00:00 PM\r\n" +
		"Status:                               Ready\r\n" +
		"Last Run Time:                        8/28/2026 8:30:00 PM\r\n" +
		"Last Result:                          0\r\n"
	st := parseSchtasksListOutput(sample)
	if !st.Registered {
		t.Error("parseSchtasksListOutput did not mark the task Registered")
	}
	if st.LastResult != "0" {
		t.Errorf("LastResult = %q, want %q", st.LastResult, "0")
	}
	if st.NextRun.IsZero() {
		t.Error("NextRun did not parse")
	}
	if st.LastRun.IsZero() {
		t.Error("LastRun did not parse")
	}
	if !st.NextRun.After(st.LastRun) {
		t.Errorf("NextRun %v should be after LastRun %v", st.NextRun, st.LastRun)
	}
}

func TestIsTaskNotFound(t *testing.T) {
	cases := []struct {
		output string
		want   bool
	}{
		{"ERROR: The system cannot find the file specified.", true},
		{"ERROR: The specified task name \"foo\" does not exist in the system.", true},
		{"SUCCESS: The scheduled task \"foo\" has successfully been created.", false},
	}
	for _, tc := range cases {
		if got := isTaskNotFound(tc.output); got != tc.want {
			t.Errorf("isTaskNotFound(%q) = %v, want %v", tc.output, got, tc.want)
		}
	}
}

func TestWindowsParseTaskInterval(t *testing.T) {
	const xmlSnippet = `<Task><Triggers><CalendarTrigger><Repetition><Interval>PT30M</Interval></Repetition></CalendarTrigger></Triggers></Task>`
	d, err := windowsParseTaskInterval(xmlSnippet)
	if err != nil {
		t.Fatalf("windowsParseTaskInterval: %v", err)
	}
	if d != 30*time.Minute {
		t.Errorf("windowsParseTaskInterval(...) = %s, want %s", d, 30*time.Minute)
	}
}

// TestTaskXMLLogonTypesDifferOnlyInTheLogonType guards the fallback: the
// InteractiveToken registration must be the same task in every other respect,
// or a user who could not get S4U silently gets a differently-behaved backup.
func TestTaskXMLLogonTypesDifferOnlyInTheLogonType(t *testing.T) {
	e := validEntry()
	s4u, err := windowsTaskXMLAs(e, "PC\\me", logonS4U)
	if err != nil {
		t.Fatal(err)
	}
	inter, err := windowsTaskXMLAs(e, "PC\\me", logonInteractiveToken)
	if err != nil {
		t.Fatal(err)
	}
	if s4u == inter {
		t.Fatal("both logon types produced identical XML")
	}
	if normalised := strings.Replace(inter, "<LogonType>"+logonInteractiveToken+"</LogonType>",
		"<LogonType>"+logonS4U+"</LogonType>", 1); normalised != s4u {
		t.Error("the two task definitions differ somewhere other than <LogonType>")
	}
	// The settings that make it a backup rather than a nuisance must be in
	// both: hidden, no time limit, and runs on battery.
	for _, want := range []string{"<Hidden>true</Hidden>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>"} {
		if !strings.Contains(inter, want) {
			t.Errorf("the fallback task lost %s", want)
		}
	}
}

func TestParseTaskLogonType(t *testing.T) {
	for _, tc := range []struct {
		xml  string
		want string
	}{
		{"<LogonType>S4U</LogonType>", "S4U"},
		{"<LogonType>InteractiveToken</LogonType>", "InteractiveToken"},
		{"  <LogonType> S4U </LogonType>\n", "S4U"},
		{"<Principal><UserId>me</UserId></Principal>", ""},
		{"", ""},
	} {
		if got := windowsParseTaskLogonType(tc.xml); got != tc.want {
			t.Errorf("windowsParseTaskLogonType(%q) = %q, want %q", tc.xml, got, tc.want)
		}
	}

	// And it reads back what the builder wrote, which is the pairing that
	// decides whether `schedule` tells the truth about signing out.
	for _, lt := range []string{logonS4U, logonInteractiveToken} {
		x, err := windowsTaskXMLAs(validEntry(), "PC\\me", lt)
		if err != nil {
			t.Fatal(err)
		}
		if got := windowsParseTaskLogonType(x); got != lt {
			t.Errorf("round trip of %s read back as %q", lt, got)
		}
	}
}

// A backup interrupted by a shutdown has an upload sitting half-finished on
// the server, and the answer to "when does that carry on?" should be "when
// you next sign in" rather than "within half an hour". StartWhenAvailable
// catches up a missed tick eventually; this makes it prompt.
func TestTheTaskAlsoRunsAtLogon(t *testing.T) {
	xml, err := windowsTaskXML(Entry{
		Name: "r2backup", Interval: 30 * time.Minute,
		BinaryPath: `C:\Program Files\r2backup\r2b.exe`, Args: []string{"backup"},
	}, `PC\sam`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<LogonTrigger>", "</LogonTrigger>"} {
		if !strings.Contains(xml, want) {
			t.Errorf("task XML is missing %q, so an interrupted upload waits for the next tick:\n%s", want, xml)
		}
	}
	// The user it runs as has to be named on the trigger too, or Task
	// Scheduler fires it for any logon on the machine.
	if strings.Count(xml, `<UserId>PC\sam</UserId>`) < 2 {
		t.Errorf("the logon trigger does not name the user:\n%s", xml)
	}
	// Signing in is the busiest the machine gets; starting a backup into the
	// middle of it is the one a user notices.
	if !strings.Contains(xml, "<Delay>PT1M</Delay>") {
		t.Errorf("the logon trigger should wait a moment before running:\n%s", xml)
	}
	// And it must not be able to stack on a run already going.
	if !strings.Contains(xml, "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>") {
		t.Errorf("a second trigger needs IgnoreNew or two runs can overlap:\n%s", xml)
	}
}
