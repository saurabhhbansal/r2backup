// Command r2b backs up folders to Cloudflare R2 and restores them anywhere.
package main

// The Windows icon for r2b.exe and r2bw.exe; see winres_gen.go.
//go:generate go run winres_gen.go

import (
	"fmt"
	"os"

	"github.com/saurabhhbansal/r2backup/internal/cli"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/selfupdate"
	"github.com/saurabhhbansal/r2backup/internal/winconsole"
)

// version is overwritten at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// First, before anything else runs: a scheduled run on Windows must not
	// leave a console window sitting on the desktop for the length of the
	// backup. Read off os.Args rather than the parsed flag because every
	// millisecond until this call is a millisecond the window could be
	// painted in -- cobra has not built a command tree yet. Root registers
	// the same flag, which is what makes it documented rather than magic.
	if winconsole.WantsHidden(os.Args[1:]) {
		winconsole.Hide()
	}
	// Next, --data-dir, before anything reads the data directory. A
	// scheduled run is started with none of the environment of the shell
	// that registered it, so this is how it finds the right one.
	config.ApplyDataDirArg(os.Args[1:])
	cli.Version = version
	// Remove the binary a previous update moved aside. Doing it here means the
	// scheduler clears it within one interval without anyone being asked to.
	// A failure is not worth stopping a backup for.
	_ = selfupdate.Cleanup()
	opts := &cli.Options{Out: os.Stdout, Err: os.Stderr}
	if err := cli.NewRoot(opts).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "r2b:", err)
		os.Exit(1)
	}
}
