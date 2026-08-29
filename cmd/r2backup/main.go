// Command r2backup backs up folders to Cloudflare R2 and restores them anywhere.
package main

import (
	"fmt"
	"os"

	"github.com/saurabhhbansal/r2backup/internal/cli"
	"github.com/saurabhhbansal/r2backup/internal/selfupdate"
)

// version is overwritten at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	// Remove the binary a previous update moved aside. Doing it here means the
	// scheduler clears it within one interval without anyone being asked to.
	// A failure is not worth stopping a backup for.
	_ = selfupdate.Cleanup()
	opts := &cli.Options{Out: os.Stdout, Err: os.Stderr}
	if err := cli.NewRoot(opts).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "r2backup:", err)
		os.Exit(1)
	}
}
