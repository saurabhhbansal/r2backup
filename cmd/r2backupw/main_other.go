//go:build !windows

// Command r2backupw exists only on Windows; see main_windows.go. This stub
// keeps `go build ./...` and `go vet ./...` working on Linux and macOS, where
// a package with every file excluded by build tags is an error rather than an
// empty package.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"r2backupw exists only on Windows, where it runs r2backup without a console window. Run r2backup directly.")
	os.Exit(1)
}
