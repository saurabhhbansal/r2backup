package schedule

import (
	"strings"
	"testing"
	"time"
)

func systemdEntry() Entry {
	return Entry{
		Name:       "r2backup-default",
		Interval:   30 * time.Minute,
		BinaryPath: "/usr/local/bin/r2backup",
		Args:       []string{"run", "--all"},
	}
}

func TestSystemdTimerUnitContents(t *testing.T) {
	unit := systemdTimerUnit(systemdEntry())
	cases := []struct {
		must string
		why  string
	}{
		{"OnUnitActiveSec=30min", "without it the timer never fires on the requested cadence at all"},
		{"Persistent=true", "without it a run missed while asleep or off is skipped instead of firing at next boot"},
		{"Unit=r2backup-default.service", "without an explicit Unit= the timer can't be told which .service to trigger"},
		{"[Timer]", "not a valid systemd timer unit without a [Timer] section"},
	}
	for _, tc := range cases {
		if !strings.Contains(unit, tc.must) {
			t.Errorf("timer unit missing %q: %s\n--- unit ---\n%s", tc.must, tc.why, unit)
		}
	}
}

func TestSystemdServiceUnitContents(t *testing.T) {
	svc := systemdServiceUnit(systemdEntry())
	if !strings.Contains(svc, "Type=oneshot") {
		t.Errorf("service unit missing Type=oneshot: r2backup ships no daemon, the process must run once and exit\n--- unit ---\n%s", svc)
	}
	wantExec := "ExecStart=" + systemdExecStart(systemdEntry().BinaryPath, systemdEntry().Args)
	if !strings.Contains(svc, wantExec) {
		t.Errorf("service unit missing %q\n--- unit ---\n%s", wantExec, svc)
	}
}

func TestSystemdExecStartQuotesSpacedPath(t *testing.T) {
	got := systemdExecStart("/home/my user/bin/r2backup", []string{"run", "--all"})
	want := `"/home/my user/bin/r2backup" run --all`
	if got != want {
		t.Errorf("systemdExecStart(spaced path) = %q, want %q", got, want)
	}
}

func TestSystemdDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Minute, "30min"},
		{2 * time.Hour, "2h"},
		{90 * time.Minute, "90min"},
	}
	for _, tc := range cases {
		if got := systemdDuration(tc.in); got != tc.want {
			t.Errorf("systemdDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSystemdDurationRoundTrips(t *testing.T) {
	for _, d := range []time.Duration{30 * time.Minute, 2 * time.Hour, 90 * time.Minute} {
		s := systemdDuration(d)
		got, err := parseSystemdDuration(s)
		if err != nil {
			t.Fatalf("parseSystemdDuration(%q): %v", s, err)
		}
		if got != d {
			t.Errorf("parseSystemdDuration(systemdDuration(%s)) = %s, want %s", d, got, d)
		}
	}
}

func TestParseSystemdTimerInterval(t *testing.T) {
	unit := systemdTimerUnit(systemdEntry())
	got, err := parseSystemdTimerInterval(unit)
	if err != nil {
		t.Fatalf("parseSystemdTimerInterval: %v", err)
	}
	if got != systemdEntry().Interval {
		t.Errorf("parseSystemdTimerInterval(...) = %s, want %s", got, systemdEntry().Interval)
	}
}

func TestSystemdQuoteArg(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"run", "run"},
		{"a user", `"a user"`},
		{"", `""`},
		{`with"quote`, `"with\"quote"`},
	}
	for _, tc := range cases {
		if got := systemdQuoteArg(tc.in); got != tc.want {
			t.Errorf("systemdQuoteArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSystemdShow(t *testing.T) {
	const sample = "ActiveState=active\nLastTriggerUSec=Fri 2026-08-28 20:00:00 UTC\nNextElapseUSecRealtime=n/a\n"
	props := parseSystemdShow(sample)
	if props["ActiveState"] != "active" {
		t.Errorf("ActiveState = %q, want active", props["ActiveState"])
	}
	if props["NextElapseUSecRealtime"] != "n/a" {
		t.Errorf("NextElapseUSecRealtime = %q, want n/a", props["NextElapseUSecRealtime"])
	}
}

func TestParseSystemdTimestamp(t *testing.T) {
	if got := parseSystemdTimestamp("n/a"); !got.IsZero() {
		t.Errorf("parseSystemdTimestamp(n/a) = %v, want zero time", got)
	}
	got := parseSystemdTimestamp("Fri 2026-08-28 20:00:00 UTC")
	if got.IsZero() {
		t.Error("parseSystemdTimestamp did not parse a well-formed systemd timestamp")
	}
}
