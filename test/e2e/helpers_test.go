package e2e

import (
	"os"
	"path/filepath"
	"time"
)

// writeRelFile writes data at root/rel, creating any missing parent
// directories, using forward slashes the same way fixtures.Build and
// scan.Key do -- rel is always slash-separated regardless of platform.
func writeRelFile(root, rel string, data []byte) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, 0o644)
}

// touchRel sets root/rel's mtime (and atime) to t.
func touchRel(root, rel string, t time.Time) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	return os.Chtimes(abs, t, t)
}

// readRelFile reads root/rel.
func readRelFile(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
}
