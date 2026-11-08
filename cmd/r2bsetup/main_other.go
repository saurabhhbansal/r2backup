//go:build !windows

// Command r2bsetup exists only on Windows. This stub keeps `go build ./...`
// working elsewhere.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "r2bsetup is the Windows installer. Elsewhere, use scripts/install.sh.")
	os.Exit(1)
}
