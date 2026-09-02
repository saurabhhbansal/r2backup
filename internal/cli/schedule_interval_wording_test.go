package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/schedule"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// TestStatusReportsTheIntervalTheWayAPersonWouldSayIt is finding M6: `status`
// used to print st.Interval straight through fmt's %s, which for a
// time.Duration means time.Duration.String() -- "47m0s" for 47 minutes.
// v1.0.5's release notes claim intervals read the way a person would say
// them, which was only true of the dashboard; this is the same promise for
// the command line.
//
// The interval is deliberately not a round number of minutes some other
// r2backup process on this machine might genuinely have registered (30, the
// package default, chief among them) -- this has to prove the assertion is
// reading back the fake this test installed, not a schedule that happens to
// already exist on whatever machine runs it.
//
// scheduleCurrent (commands.go) is the seam that lets this be driven through
// printStatus without touching whatever scheduler this test happens to run
// under -- see schedule_repair_test.go for the same pattern.
func TestStatusReportsTheIntervalTheWayAPersonWouldSayIt(t *testing.T) {
	origCurrent := scheduleCurrent
	t.Cleanup(func() { scheduleCurrent = origCurrent })
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 47 * time.Minute}, nil
	}

	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := a.sets.Add(sets.Set{Name: "Documents", Root: t.TempDir(), Machine: "testpc"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printStatus(a, &Options{Out: &out}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "47m0s") {
		t.Errorf("status printed the raw time.Duration spelling, got:\n%s", got)
	}
	if !strings.Contains(got, "47 minutes") {
		t.Errorf("status should say \"47 minutes\", the way a person would say it, got:\n%s", got)
	}
}

// TestAddOfferReportsTheIntervalTheWayAPersonWouldSayIt is the same finding
// (M6) at the other site named in it: the offer `add` makes when a schedule
// already exists.
func TestAddOfferReportsTheIntervalTheWayAPersonWouldSayIt(t *testing.T) {
	origCurrent := scheduleCurrent
	t.Cleanup(func() { scheduleCurrent = origCurrent })
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 47 * time.Minute}, nil
	}

	var out bytes.Buffer
	offerSchedule(&Options{Out: &out}, 30)

	got := out.String()
	if strings.Contains(got, "47m0s") {
		t.Errorf("the add offer printed the raw time.Duration spelling, got:\n%s", got)
	}
	if !strings.Contains(got, "47 minutes") {
		t.Errorf("the add offer should say \"47 minutes\", the way a person would say it, got:\n%s", got)
	}
}

// TestStatusOnAZeroIntervalDoesNotFabricateOne covers the second half of
// M6: on the crontab fallback (internal/schedule/schedule_linux.go's
// Current, past the point where no systemd unit file exists), Registered
// comes back true with a zero Interval -- cron has no notion of "every N
// minutes" for this package to read back. Printing "every 0s" states a
// number nobody configured; the fix has to say something true instead.
func TestStatusOnAZeroIntervalDoesNotFabricateOne(t *testing.T) {
	origCurrent := scheduleCurrent
	t.Cleanup(func() { scheduleCurrent = origCurrent })
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 0}, nil
	}

	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := a.sets.Add(sets.Set{Name: "Documents", Root: t.TempDir(), Machine: "testpc"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printStatus(a, &Options{Out: &out}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "every 0s") || strings.Contains(got, "0 minutes") || strings.Contains(got, "0 sec") {
		t.Errorf("a scheduler that cannot report its interval must not fabricate one, got:\n%s", got)
	}
	if !strings.Contains(got, "Scheduled") {
		t.Errorf("status should still say it is scheduled, got:\n%s", got)
	}
}

// TestAddOfferOnAZeroIntervalDoesNotFabricateOne is the zero-interval case
// (see above) at the add-offer site.
func TestAddOfferOnAZeroIntervalDoesNotFabricateOne(t *testing.T) {
	origCurrent := scheduleCurrent
	t.Cleanup(func() { scheduleCurrent = origCurrent })
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 0}, nil
	}

	var out bytes.Buffer
	offerSchedule(&Options{Out: &out}, 30)

	got := out.String()
	if strings.Contains(got, "every 0s") || strings.Contains(got, "0 minutes") || strings.Contains(got, "0 sec") {
		t.Errorf("a scheduler that cannot report its interval must not fabricate one, got:\n%s", got)
	}
	if !strings.Contains(got, "already run") {
		t.Errorf("the offer should still say backups already run, got:\n%s", got)
	}
}
