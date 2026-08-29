package schedule

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func launchdEntry() Entry {
	return Entry{
		Name:       "r2backup-default",
		Interval:   30 * time.Minute,
		BinaryPath: "/Applications/r2backup.app/Contents/MacOS/r2backup",
		Args:       []string{"run", "--all"},
	}
}

func TestLaunchdPlistContents(t *testing.T) {
	plist, err := launchdPlist(launchdEntry(), "/tmp/out.log", "/tmp/err.log")
	if err != nil {
		t.Fatalf("launchdPlist: %v", err)
	}
	cases := []struct {
		must string
		why  string
	}{
		{"<key>StartInterval</key>", "without it launchd has no cadence to run on at all"},
		{"<integer>1800</integer>", "30 minutes must render as 1800 seconds, the only unit StartInterval accepts"},
		{"<false/>", "RunAtLoad must be false: loading the agent must not itself trigger an immediate backup"},
		{"<key>RunAtLoad</key>", "without the RunAtLoad key launchd defaults to running at load, which we don't want"},
		{"<key>StandardOutPath</key>", "without it a failing scheduled run leaves no trace anywhere -- launchd gives an agent no console"},
		{"<key>StandardErrorPath</key>", "same as StandardOutPath, for stderr"},
	}
	for _, tc := range cases {
		if !strings.Contains(plist, tc.must) {
			t.Errorf("plist missing %q: %s\n--- plist ---\n%s", tc.must, tc.why, plist)
		}
	}
}

func TestLaunchdPlistParsesAsXML(t *testing.T) {
	plist, err := launchdPlist(launchdEntry(), "/tmp/out.log", "/tmp/err.log")
	if err != nil {
		t.Fatalf("launchdPlist: %v", err)
	}
	var v any
	if err := xml.Unmarshal([]byte(plist), &v); err != nil {
		t.Fatalf("generated plist does not parse as XML: %v\n--- plist ---\n%s", err, plist)
	}
}

func TestLaunchdPlistArgumentsAreSeparateElements(t *testing.T) {
	e := launchdEntry()
	e.BinaryPath = "/Applications/r2 backup.app/Contents/MacOS/r2backup"
	e.Args = []string{"--data-dir", "/Users/a user/Library/Application Support/r2backup"}
	plist, err := launchdPlist(e, "/tmp/out.log", "/tmp/err.log")
	if err != nil {
		t.Fatalf("launchdPlist: %v", err)
	}
	// Unlike Windows or systemd, launchd never re-parses a joined command
	// line: each argument is its own <string> in the array, so a space in a
	// path can't merge or split anything and needs no shell-style quoting --
	// only XML escaping, which the fixture below doesn't require.
	for _, want := range []string{
		"<string>/Applications/r2 backup.app/Contents/MacOS/r2backup</string>",
		"<string>--data-dir</string>",
		"<string>/Users/a user/Library/Application Support/r2backup</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing argument element %q\n--- plist ---\n%s", want, plist)
		}
	}
}

func TestLaunchdParseStartIntervalRoundTrips(t *testing.T) {
	for _, d := range []time.Duration{30 * time.Minute, 2 * time.Hour, 90 * time.Minute} {
		e := launchdEntry()
		e.Interval = d
		plist, err := launchdPlist(e, "/tmp/out.log", "/tmp/err.log")
		if err != nil {
			t.Fatalf("launchdPlist: %v", err)
		}
		got, err := launchdParseStartInterval(plist)
		if err != nil {
			t.Fatalf("launchdParseStartInterval: %v", err)
		}
		if got != d {
			t.Errorf("launchdParseStartInterval(...) = %s, want %s", got, d)
		}
	}
}

func TestLaunchdLabel(t *testing.T) {
	if got, want := launchdLabel("r2backup-default"), "com.r2backup.r2backup-default"; got != want {
		t.Errorf("launchdLabel(...) = %q, want %q", got, want)
	}
}

func TestLaunchdPlistRejectsInvalidEntry(t *testing.T) {
	if _, err := launchdPlist(Entry{}, "/tmp/out.log", "/tmp/err.log"); err == nil {
		t.Error("launchdPlist(Entry{}, ...) = nil error, want validation to reject an empty Entry")
	}
}

func TestParseLaunchctlListStatus(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1234\t0\tcom.r2backup.default\n", "0"},
		{"-\t0\tcom.r2backup.default\n", "0"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := parseLaunchctlListStatus(tc.in); got != tc.want {
			t.Errorf("parseLaunchctlListStatus(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
