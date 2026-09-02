package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
)

// deviceServer returns a fake account service that answers GET /devices with
// one device whose last_seen is lastSeen (Unix seconds, matching what
// worker/src/index.ts's ctx.now actually writes) and GET /vault with 404, so
// callers that also fetch the vault see "nothing stored" rather than an
// error.
func deviceServer(t *testing.T, lastSeen int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/devices" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"devices": []map[string]any{
					{"device_name": "Laptop", "os": "windows", "last_seen": lastSeen},
				},
			})
		case r.URL.Path == "/vault" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestAccountDevicesShowsARealLastSeenDate is the regression guard for H2:
// the Worker stamps last_seen in Unix SECONDS (worker/src/index.ts's
// ctx.now is `Math.floor(Date.now() / 1000)`, and worker/migrations stores
// it as a plain INTEGER with no unit conversion), but this command used to
// decode it with time.UnixMilli, so every device printed as sometime in
// January 1970. A fixture of 42 renders as 1970 under either unit and would
// never have caught that, so this uses a real timestamp -- 15 March 2024 --
// and checks the actual date the command prints, not just that it printed
// something.
func TestAccountDevicesShowsARealLastSeenDate(t *testing.T) {
	lastSeen := time.Date(2024, time.March, 15, 9, 30, 0, 0, time.UTC).Unix()
	srv := deviceServer(t, lastSeen)
	defer srv.Close()

	t.Setenv(EnvAccountAPI, srv.URL)
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	if err := account.SaveToken("fake.token.value"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	var out, errOut bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &errOut})
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"account", "devices"})
	if err := root.Execute(); err != nil {
		t.Fatalf("account devices: %v\n--- output ---\n%s", err, out.String())
	}

	got := out.String()
	// Same format string as internal/cli/account.go's devices command.
	want := time.Unix(lastSeen, 0).Format("2 Jan 15:04")
	if !strings.Contains(got, want) {
		t.Errorf("output = %q, want it to contain last-seen date %q", got, want)
	}
	if strings.Contains(got, "1970") {
		t.Errorf("output = %q, printed a 1970 date -- last_seen was decoded as milliseconds", got)
	}
}

// TestDashboardAccountTabShowsARealLastSeenDate is the same regression guard
// for the Account tab (internal/cli/dashboard_ops.go's Account method,
// rendered by internal/ui/view.go with the identical "2 Jan 15:04" format).
// It exercises dashboard.Account directly rather than the TUI's render loop,
// but formats the resulting time.Time the same way the view does, so a
// return to time.UnixMilli here fails it exactly as it would fail a person
// looking at the tab.
func TestDashboardAccountTabShowsARealLastSeenDate(t *testing.T) {
	lastSeen := time.Date(2024, time.March, 15, 9, 30, 0, 0, time.UTC).Unix()
	srv := deviceServer(t, lastSeen)
	defer srv.Close()

	t.Setenv(EnvAccountAPI, srv.URL)
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	if err := account.SaveToken("fake.token.value"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	d, err := openDashboard(&Options{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	v, err := d.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if len(v.Devices) != 1 {
		t.Fatalf("Devices = %+v, want one entry", v.Devices)
	}

	// Same format string as internal/ui/view.go's device row.
	got := v.Devices[0].LastSeen.Format("2 Jan 15:04")
	want := time.Unix(lastSeen, 0).Format("2 Jan 15:04")
	if got != want {
		t.Errorf("LastSeen formatted as %q, want %q", got, want)
	}
	if v.Devices[0].LastSeen.Year() == 1970 {
		t.Errorf("LastSeen = %v, landed in 1970 -- last_seen was decoded as milliseconds", v.Devices[0].LastSeen)
	}
}
