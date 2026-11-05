package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
)

// TestMain points the data directory somewhere disposable before any test
// runs. Commands here read and write the real one by default, so a test that
// forgot to override it would back up -- or change -- the sets of whoever is
// running the tests. Tests that need their own directory still set it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "r2backup-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv(config.EnvDataDir, dir)
	// Nothing a test does may register a real scheduled task or sign-in entry.
	firstRunDefaults = func() {}
	// The panel preview (TestPanelPreview) once left a real Task Scheduler
	// entry pointing at the deleted test binary. Tests that check scheduling
	// put their own fakes in these seams.
	scheduleInstall = func(schedule.Entry) error { return nil }
	scheduleRemove = func(string) error { return nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
