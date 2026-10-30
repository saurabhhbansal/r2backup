//go:build ignore

// winres_gen builds the Windows resources -- the icon, and the name Task
// Manager shows -- into r2b.exe and r2bw.exe. Run it with `go generate
// ./cmd/r2b` after changing the mark in internal/tray/icon.go; it writes
// rsrc_windows_*.syso beside each program's main package, which go build
// links in by itself, with or without cgo.
//
// The icon is drawn by the same code as the taskbar icon, so the two cannot
// drift apart.
package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/saurabhhbansal/r2backup/internal/tray"
)

func main() {
	tmp, err := os.MkdirTemp("", "r2b-winres")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	ico := filepath.Join(tmp, "r2backup.ico")
	if err := os.WriteFile(ico, tray.AppIcon(), 0o644); err != nil {
		log.Fatal(err)
	}
	for _, p := range []struct{ dir, desc string }{
		{".", "r2backup"},
		// r2bw is what the Start menu shortcut and the sign-in entry run.
		{"../r2bw", "r2backup launcher"},
	} {
		cmd := exec.Command("go", "run", "github.com/tc-hib/go-winres@v0.3.3", "simply",
			"--icon", ico, "--manifest", "none", "--arch", "amd64,arm64",
			"--product-name", "r2backup", "--file-description", p.desc,
			"--out", filepath.Join(p.dir, "rsrc"))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			log.Fatalf("go-winres for %s: %v", p.dir, err)
		}
	}
}
