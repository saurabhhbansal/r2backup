package main

import (
	"os"
	"strings"
	"testing"
)

// The entire fix is one linker flag. Built without -H=windowsgui, r2backupw
// becomes an ordinary console binary, Windows allocates a console for it, and
// the scheduled run puts a window on the desktop again -- while everything
// still compiles, every other test still passes, and `schedule` still says
// "out of sight". Nothing else in the project would notice, so this does.
//
// It reads the release config rather than inspecting a built artifact,
// because the realistic way to lose this is someone editing the yaml, and a
// test that builds with the flag it is checking would only be testing itself.
func TestTheLauncherIsReleasedWithoutAConsole(t *testing.T) {
	data, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("read the release config: %v", err)
	}
	cfg := string(data)

	idx := strings.Index(cfg, "id: r2backupw")
	if idx < 0 {
		t.Fatal("no r2backupw build in .goreleaser.yaml: the launcher would not ship at all, and every Windows schedule would fall back to a visible console")
	}
	// The build entry runs until the next top-level key.
	block := cfg[idx:]
	if end := strings.Index(block, "\narchives:"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "-H=windowsgui") {
		t.Error("the r2backupw build has lost -H=windowsgui; it would be a console binary and the console window comes back")
	}
	if !strings.Contains(block, "goos: [windows]") {
		t.Error("the r2backupw build no longer targets windows only")
	}

	// It has to actually reach the archive, which is a separate mistake:
	// goreleaser builds every id whether or not an archive includes it.
	if !strings.Contains(cfg, "ids: [r2backup, r2backupw]") {
		t.Error("the archive no longer carries both binaries; the launcher would be built and never shipped")
	}
}

// The Windows installer has to place the launcher too. It did not, and the
// fix would have shipped in every archive and reached nobody who used the
// one-liner: they would have got a correct product whose scheduled runs pop a
// console window, for a file that was sitting in the archive the whole time.
func TestTheWindowsInstallerPlacesTheLauncher(t *testing.T) {
	data, err := os.ReadFile("../../scripts/install.ps1")
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	if !strings.Contains(string(data), "r2backupw.exe") {
		t.Error("install.ps1 never mentions r2backupw.exe, so an installed r2backup would show a console window on every scheduled run")
	}
}
