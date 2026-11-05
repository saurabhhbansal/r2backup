package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
)

func TestParseEvery(t *testing.T) {
	good := map[string]int{"45m": 45, "45": 45, "2h": 120, "1h30m": 90, " 5m ": 5, "24h": 1440}
	for in, want := range good {
		got, err := ParseEvery(in)
		if err != nil || got != want {
			t.Errorf("ParseEvery(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "4m", "25h", "soon", "30s", "1.5m"} {
		if n, err := ParseEvery(in); err == nil {
			t.Errorf("ParseEvery(%q) = %d, want an error", in, n)
		}
	}
}

func runSchedule(t *testing.T, args ...string) (schedule.Entry, string, error) {
	t.Helper()
	origCurrent, origInstall, origRemove := scheduleCurrent, scheduleInstall, scheduleRemove
	t.Cleanup(func() { scheduleCurrent, scheduleInstall, scheduleRemove = origCurrent, origInstall, origRemove })
	var got schedule.Entry
	scheduleCurrent = func(string) (schedule.Status, error) { return schedule.Status{}, nil }
	scheduleInstall = func(e schedule.Entry) error { got = e; return nil }
	var out bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &out})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"schedule"}, args...))
	err := root.Execute()
	return got, out.String(), err
}

func TestScheduleEveryAndDailyCommands(t *testing.T) {
	if !schedule.Supported() {
		t.Skip("no scheduler on this platform")
	}
	e, _, err := runSchedule(t, "every", "45m")
	if err != nil || e.Interval.Minutes() != 45 || e.DailyAt != "" {
		t.Fatalf("every 45m: entry %+v, err %v", e, err)
	}
	e, out, err := runSchedule(t, "daily", "9:30")
	if err != nil || e.DailyAt != "09:30" {
		t.Fatalf("daily 9:30: entry %+v, err %v", e, err)
	}
	if want := "once a day at 09:30"; !strings.Contains(out, want) {
		t.Errorf("output %q should say %q", out, want)
	}
	if _, _, err := runSchedule(t, "daily", "25:00"); err == nil {
		t.Error("daily 25:00 should be refused")
	}
	if _, _, err := runSchedule(t, "every", "2m"); err == nil {
		t.Error("every 2m should be refused")
	}
}

func TestRepairKeepsADailyTime(t *testing.T) {
	origCurrent, origInstall := scheduleCurrent, scheduleInstall
	t.Cleanup(func() { scheduleCurrent, scheduleInstall = origCurrent, origInstall })
	var got schedule.Entry
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, DailyAt: "21:30"}, nil
	}
	scheduleInstall = func(e schedule.Entry) error { got = e; return nil }
	var out bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &out})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"schedule", "--repair"})
	if err := root.Execute(); err != nil || got.DailyAt != "21:30" {
		t.Fatalf("repair: entry %+v, err %v", got, err)
	}
}

func TestFirstSetupTurnsOnTheDefaultsOnlyOnce(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	var n int
	old := firstRunDefaults
	firstRunDefaults = func() { n++ }
	t.Cleanup(func() { firstRunDefaults = old })
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	c := b2("first")
	if err := a.saveCreds(c); err != nil || n != 1 {
		t.Fatalf("first save: err %v, defaults applied %d times, want 1", err, n)
	}
	if err := a.saveCreds(c); err != nil || n != 1 {
		t.Fatalf("second save: err %v, defaults applied %d times, want still 1", err, n)
	}
}
