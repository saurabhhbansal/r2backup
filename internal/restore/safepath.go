package restore

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// safeJoin resolves relKey -- an object key with the "<prefix>/current/"
// (or trash) portion already stripped, forward-slashed -- onto target,
// refusing to produce a path outside it.
//
// This program is, today, the only thing that ever writes these keys. That
// is exactly why this check exists rather than being skipped: trusting a
// value because of who currently produces it is how "we wrote every key"
// quietly turns into "anyone who can write one object to this bucket can
// write anywhere the restoring user's account can" the day something else
// touches the bucket -- a bug in a future version, a person poking at it
// with a different tool, a compromised credential. A key must earn its way
// onto disk on every restore, not just the ones written by code that still
// remembers to be careful.
func safeJoin(target, relKey string) (string, error) {
	if relKey == "" {
		return "", fmt.Errorf("restore: empty key")
	}
	// A literal backslash is rejected outright rather than treated as a
	// separator. On Windows, filepath.Join would split on it like any
	// other path element -- silently reinterpreting "..\\..\\etc\\passwd"
	// as a traversal instead of a filename containing backslashes. On
	// Unix a backslash is a perfectly legal filename character, so
	// treating it as one here is also the only reading that is correct on
	// both platforms at once.
	if strings.Contains(relKey, "\\") {
		return "", fmt.Errorf("restore: key %q contains a backslash, which is refused rather than reinterpreted as a path separator", relKey)
	}
	if path.IsAbs(relKey) {
		return "", fmt.Errorf("restore: key %q is an absolute path", relKey)
	}
	// A Windows drive letter ("C:...") is not caught by path.IsAbs, which
	// only knows about a leading "/". filepath.Join on Windows would
	// still treat "C:" as part of a relative path segment rather than a
	// drive root, but refusing it here means the same key is rejected
	// identically on every platform the restore might run on, rather than
	// behaving differently depending on which OS happens to be restoring.
	if len(relKey) >= 2 && relKey[1] == ':' {
		return "", fmt.Errorf("restore: key %q looks like a Windows drive-qualified path", relKey)
	}

	cleaned := path.Clean(relKey)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("restore: key %q escapes the restore target", relKey)
	}

	full := filepath.Join(target, filepath.FromSlash(cleaned))

	// Belt and braces: confirm the joined path is still lexically inside
	// target even after every check above. This is what catches a shape
	// none of the specific checks anticipated -- a future platform's own
	// idea of a path separator, say -- rather than trusting that the list
	// of individual rejections above is complete forever.
	rel, err := filepath.Rel(target, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("restore: key %q escapes the restore target", relKey)
	}
	return full, nil
}
