package scan

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Key converts an absolute filesystem path into the object key used in the
// bucket: relative to root, forward-slashed, and NFC-normalized.
//
// This is the single place path spelling is decided. It exists as one function
// on purpose. The predecessor to this tool compared paths that had been spelled
// two different ways -- macOS hands back NFD for the same filename Windows and
// Linux report as NFC, so "résumé.txt" written on one machine did not match
// itself on another, and every run re-uploaded it. Normalizing in one chokepoint
// is what stops that being possible.
func Key(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("relativize %q against %q: %w", path, root, err)
	}
	if rel == "." {
		return "", nil
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path %q escapes root %q", path, root)
	}
	return norm.NFC.String(rel), nil
}

// NormalizeKey brings a key received from elsewhere -- a bucket listing, a
// stored index, a user-supplied filter -- into the same spelling Key produces,
// so the two can be compared.
func NormalizeKey(key string) string {
	return norm.NFC.String(filepath.ToSlash(key))
}
