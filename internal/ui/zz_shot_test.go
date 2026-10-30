package ui

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestZZScreenshot writes the home screen, with colour, to a file so the
// README's picture is a real frame of the program rather than a mock-up. It
// only runs when asked for by name.
func TestZZScreenshot(t *testing.T) {
	if os.Getenv("R2B_SHOT") == "" {
		t.Skip("set R2B_SHOT to a path, and R2B_SHOT_VERSION, to write the screenshot frame")
	}
	if os.Getenv("R2B_SHOT_VERSION") == "" {
		t.Fatal("set R2B_SHOT_VERSION: the header shows it, and a picture with no version is what this was regenerated to fix")
	}
	b := shotBackend()
	m := sized(b, 150, 44)
	if err := os.WriteFile(os.Getenv("R2B_SHOT"), []byte(m.View()), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Println("wrote", os.Getenv("R2B_SHOT"))
}

// TestZZScreens writes one frame per mode, in colour, into the directory
// R2B_SHOTS names: the way to look at the whole interface at once when its
// design changes.
func TestZZScreens(t *testing.T) {
	dir := os.Getenv("R2B_SHOTS")
	if dir == "" {
		t.Skip("set R2B_SHOTS to a directory to write one frame per mode")
	}
	lipgloss.SetColorProfile(termenv.ANSI256)
	lipgloss.SetHasDarkBackground(true)
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		for tb := tab(0); tb < numTabs; tb++ {
			m := sized(shotBackend(), size[0], size[1])
			m.tab = tb
			m.layout()
			name := fmt.Sprintf("%s/%dx%d-%d-%s.ans", dir, size[0], size[1], int(tb)+1, tb)
			if err := os.WriteFile(name, []byte(m.View()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The variants: a computer backing up to Drime, and one not set up.
	extra := map[string]func(*fakeBackend){
		"drime": func(b *fakeBackend) { b.ov.Provider, b.ov.Bucket = "drime", "Drime · r2backup" },
		"fresh": func(b *fakeBackend) { b.ov.Configured, b.ov.Bucket, b.sets = false, "", nil },
	}
	for label, change := range extra {
		for _, tb := range []tab{tabFolders, tabCost, tabAccount} {
			b := shotBackend()
			change(b)
			m := sized(b, 120, 36)
			m.tab = tb
			m.layout()
			name := fmt.Sprintf("%s/zz-%s-%d-%s.ans", dir, label, int(tb)+1, tb)
			if err := os.WriteFile(name, []byte(m.View()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func shotBackend() *fakeBackend {
	if os.Getenv("R2B_SHOT_VERSION") == "" {
		os.Setenv("R2B_SHOT_VERSION", "dev")
	}
	return &fakeBackend{
		sets: []SetView{
			{Name: "Documents", Root: "/home/sam/Documents", Prefix: "machines/studio/Documents",
				State: "ok", HasRun: true, LastRun: time.Now().Add(-22 * time.Minute),
				Uploaded: 4, Unchanged: 1841, Bytes: 12_600_000, Operations: 9, Objects: 1845, Retention: 30},
			{Name: "Photos", Root: "/home/sam/Pictures", Prefix: "machines/studio/Photos",
				State: "ok", HasRun: true, LastRun: time.Now().Add(-22 * time.Minute),
				Unchanged: 20418, Objects: 20418, Retention: 30},
			{Name: "Code", Root: "/home/sam/code", Prefix: "machines/studio/Code",
				State: "ok", HasRun: true, LastRun: time.Now().Add(-4 * time.Hour),
				Uploaded: 61, Unchanged: 8022, Bytes: 3_400_000, Operations: 122, Objects: 8083,
				Excludes: []string{"node_modules"}, Retention: 30},
			{Name: "Server backups", Root: "/srv/dumps", Prefix: "machines/studio/Server backups",
				State: "never run", Retention: 30},
		},
		ov: Overview{Machine: "studio", Bucket: "sam-backups", Configured: true,
			// The header shows which build it is, so the picture has to say
			// a real one. Passed in rather than hard-coded here: this file
			// cannot import internal/cli, which imports this package, and a
			// version frozen in a test is a version that goes stale at the
			// next release with nothing to catch it.
			Version:            os.Getenv("R2B_SHOT_VERSION"),
			OpsUsed:            4127,
			OpsLimit:           1000000,
			Scheduled:          true,
			Interval:           30 * time.Minute,
			SchedulerAvailable: true},
	}
}
