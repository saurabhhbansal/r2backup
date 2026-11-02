//go:build !windows

package cli

// platformDrimeSyncRoots: Drime's synced folder is looked for on Windows
// only, where its app runs.
func platformDrimeSyncRoots() []string { return nil }
