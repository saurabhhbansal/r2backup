package cli

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// TestLoadPopulatesConfigured is the guard for the bug fixed here:
// ui.Overview.Configured was declared, read by the Folders and Account tabs,
// and never assigned by the only real ui.Backend -- so a fully set-up
// machine still told the user to go sign in, while the header line right
// above it printed the real machine name and bucket.
//
// ui.Overview.Version had the identical defect, fixed in v1.0.4. The pattern
// is "a field the real backend forgets to populate", and it's invisible to
// every test that only exercises a fake Overview, because a fake always sets
// every field it's handed by hand. This drives the real dashboard -- the
// only thing that ever builds an Overview for a person to look at -- and
// checks what a freshly configured machine can actually promise.
func TestLoadPopulatesConfigured(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	c := creds.Credentials{
		AccountID: "acct", AccessKeyID: "key", SecretAccessKey: "secret",
		Bucket: "my-bucket",
	}
	if err := a.creds.Save(c); err != nil {
		t.Fatal(err)
	}
	a.close()

	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	_, ov, err := d.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ov.Configured {
		t.Error("Configured = false with a valid credential set saved, want true")
	}
	if ov.Bucket != c.Bucket {
		t.Errorf("Bucket = %q, want %q", ov.Bucket, c.Bucket)
	}
	if ov.Machine == "" {
		t.Error("Machine is empty")
	}
	if ov.Version == "" {
		t.Error("Version is empty")
	}
	if ov.OpsLimit == 0 {
		t.Error("OpsLimit is zero, so the footer can't show the free-tier limit")
	}

	assertOverviewFullyPopulated(t, ov)
}

// TestLoadReportsUnconfigured is the negative half: a machine with nothing
// saved has to say so, not merely fail to say the opposite. Bucket empty is
// as load-bearing as Configured false -- it's what stops the header showing
// a name that came from nowhere.
func TestLoadReportsUnconfigured(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	_, ov, err := d.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ov.Configured {
		t.Error("Configured = true with no credentials saved, want false")
	}
	if ov.Bucket != "" {
		t.Errorf("Bucket = %q with no credentials saved, want empty", ov.Bucket)
	}
}

// overviewZeroOnAFreshlyConfiguredMachine names every ui.Overview field that
// genuinely has nothing to report right after credentials are saved: no set
// has ever run, nothing is scheduled, nothing is mid-upload, nothing is
// running. Anything not named here is a field dashboard.Load is expected to
// have filled in by that point.
//
// This is deliberately an allow-list rather than the other way round: a
// naive "every field must be non-zero" assertion would be wrong the moment
// it was written, since half of Overview is correctly zero on a fresh
// install. The allow-list means adding a field to ui.Overview without either
// populating it in Load or adding it here, with a reason, fails this test --
// which is exactly the silence that let Configured and Version both ship
// unassigned.
var overviewZeroOnAFreshlyConfiguredMachine = map[string]string{
	"OpsUsed": "no operation has been billed against a brand new month yet",

	"Scheduled":         "no OS scheduler entry has been registered",
	"Interval":          "meaningless without Scheduled",
	"NextRun":           "meaningless without Scheduled",
	"LastRun":           "meaningless without Scheduled",
	"RunsWhenSignedOut": "meaningless without Scheduled",

	"Running":    "no backup is in progress",
	"RunPercent": "meaningless without Running",
	"RunETA":     "meaningless without Running",

	"Interrupted":      "no run has ever been cut off",
	"InterruptedAt":    "meaningless without Interrupted",
	"InterruptedDone":  "meaningless without Interrupted",
	"InterruptedTotal": "meaningless without Interrupted",

	"PendingDone":  "no large upload has ever been started",
	"PendingTotal": "meaningless without PendingDone",
	"PendingFiles": "meaningless without PendingDone",
}

// assertOverviewFullyPopulated walks every field of ui.Overview by name and
// fails on any zero value not accounted for in
// overviewZeroOnAFreshlyConfiguredMachine. It runs against the real
// dashboard's output, not a fake, because a fake is exactly what let the
// last two instances of this defect through.
func assertOverviewFullyPopulated(t *testing.T, ov ui.Overview) {
	t.Helper()
	v := reflect.ValueOf(ov)
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		if _, ok := overviewZeroOnAFreshlyConfiguredMachine[f.Name]; ok {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("ui.Overview.%s is zero on a machine with credentials saved -- "+
				"either dashboard.Load needs to populate it, or it belongs in "+
				"overviewZeroOnAFreshlyConfiguredMachine with a reason", f.Name)
		}
	}
}
